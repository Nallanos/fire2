// Package thin creates and removes sandbox root drives in the fire2 dm-thin
// pool, in pure Go: device-mapper ioctls, no dmsetup, no shell, no sudo. The
// worker runs as root.
//
// Each sandbox volume is a dm-thin snapshot of the pool's base volume (the
// imported rootfs.ext4): metadata only, a few ms, and blocks are allocated
// only when the guest writes to them. It is exposed as
// /dev/mapper/fire2-sb-<sandbox-id>.
//
// The pool itself and its base volume are set up on the cold path by
// fire2-thinpool-up and fire2-thinpool-import-base (Ansible).
package thin

import (
	"bufio"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// DefaultConfPath is the pool settings file Ansible deploys, shared with the
// cold-path scripts.
const DefaultConfPath = "/etc/fire2/thinpool.conf"

const (
	volumePrefix = "fire2-sb-"
	mapperDir    = "/dev/mapper"
	// Thin volume IDs are 24-bit.
	maxThinID = 1<<24 - 1
	// createAttempts bounds the random thin ID draws on collision.
	createAttempts = 16
)

// validSandboxID keeps volume names to what a device-mapper name and a path
// can safely hold.
var validSandboxID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Pool is the fire2 dm-thin pool, as described by thinpool.conf.
type Pool struct {
	Name string // POOL_NAME: the pool's device-mapper name
	Dir  string // POOL_DIR: holds base.env
	// FirstSandboxID is the lowest thin ID of a sandbox volume. Base volumes
	// sit below it; everything at or above it is disposable.
	FirstSandboxID uint32
}

// LoadPool reads the pool settings from confPath.
func LoadPool(confPath string) (Pool, error) {
	vars, err := readEnvFile(confPath)
	if err != nil {
		return Pool{}, err
	}
	p := Pool{Name: vars["POOL_NAME"], Dir: vars["POOL_DIR"]}
	if p.Name == "" || p.Dir == "" {
		return Pool{}, fmt.Errorf("%s: POOL_NAME and POOL_DIR are required", confPath)
	}
	first, err := strconv.ParseUint(vars["FIRST_SANDBOX_ID"], 10, 32)
	if err != nil || first == 0 || first > maxThinID {
		return Pool{}, fmt.Errorf("%s: invalid FIRST_SANDBOX_ID %q", confPath, vars["FIRST_SANDBOX_ID"])
	}
	p.FirstSandboxID = uint32(first)
	return p, nil
}

// base is the volume sandbox volumes are snapshotted from, recorded in
// base.env by fire2-thinpool-import-base. It is read on every create: an
// import swaps it while the worker runs.
type base struct {
	thinID  uint32
	sectors uint64
}

func (p Pool) loadBase() (base, error) {
	path := filepath.Join(p.Dir, "base.env")
	vars, err := readEnvFile(path)
	if err != nil {
		return base{}, fmt.Errorf("no base volume imported: %w", err)
	}
	id, err := strconv.ParseUint(vars["BASE_THIN_ID"], 10, 32)
	if err != nil || id == 0 || id >= uint64(p.FirstSandboxID) {
		return base{}, fmt.Errorf("%s: invalid BASE_THIN_ID %q", path, vars["BASE_THIN_ID"])
	}
	sectors, err := strconv.ParseUint(vars["BASE_SECTORS"], 10, 64)
	if err != nil || sectors == 0 {
		return base{}, fmt.Errorf("%s: invalid BASE_SECTORS %q", path, vars["BASE_SECTORS"])
	}
	return base{thinID: uint32(id), sectors: sectors}, nil
}

func volumeName(sandboxID string) (string, error) {
	if !validSandboxID.MatchString(sandboxID) {
		return "", fmt.Errorf("invalid sandbox id %q", sandboxID)
	}
	return volumePrefix + sandboxID, nil
}

// VolumePath is where a sandbox's volume is exposed.
func VolumePath(sandboxID string) string {
	return filepath.Join(mapperDir, volumePrefix+sandboxID)
}

// CreateVolume snapshots the base volume for sandboxID and returns the block
// device to give Firecracker. It fails if the volume already exists.
func (p Pool) CreateVolume(sandboxID string) (string, error) {
	name, err := volumeName(sandboxID)
	if err != nil {
		return "", err
	}
	b, err := p.loadBase()
	if err != nil {
		return "", err
	}
	c, err := openControl()
	if err != nil {
		return "", err
	}
	defer c.Close()

	// The kernel is the source of truth for which IDs are taken: draw one at
	// random and retry on collision. Nothing to keep in sync.
	thinID, err := p.createSnapshot(c, b.thinID)
	if err != nil {
		return "", err
	}

	dev, err := c.createDevice(name, []target{{
		start:  0,
		length: b.sectors,
		typ:    "thin",
		params: fmt.Sprintf("%s %d", filepath.Join(mapperDir, p.Name), thinID),
	}})
	if err != nil {
		if delErr := c.message(p.Name, 0, fmt.Sprintf("delete %d", thinID)); delErr != nil {
			err = errors.Join(err, delErr)
		}
		return "", err
	}

	path := VolumePath(sandboxID)
	if err := ensureNode(path, dev); err != nil {
		if rmErr := p.RemoveVolume(sandboxID); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
		return "", err
	}
	return path, nil
}

func (p Pool) createSnapshot(c *control, baseID uint32) (uint32, error) {
	for range createAttempts {
		id := p.FirstSandboxID + rand.Uint32N(maxThinID-p.FirstSandboxID+1)
		err := c.message(p.Name, 0, fmt.Sprintf("create_snap %d %d", id, baseID))
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return 0, err
		}
		return id, nil
	}
	return 0, fmt.Errorf("no free thin id after %d attempts", createAttempts)
}

// RemoveVolume removes sandboxID's volume and frees its blocks in the pool.
// It is idempotent: a volume already gone is not an error. Stop the VM first.
func (p Pool) RemoveVolume(sandboxID string) error {
	name, err := volumeName(sandboxID)
	if err != nil {
		return err
	}
	c, err := openControl()
	if err != nil {
		return err
	}
	defer c.Close()

	path := VolumePath(sandboxID)
	table, err := c.table(name)
	if errors.Is(err, unix.ENXIO) {
		return removeNode(path) // already gone
	}
	if err != nil {
		return err
	}
	thinID, err := p.sandboxThinID(name, table)
	if err != nil {
		return err
	}

	if err := c.removeDevice(name); err != nil {
		return err
	}
	if err := removeNode(path); err != nil {
		return err
	}
	// If this fails, the thin ID stays allocated until the next boot, when
	// fire2-thinpool-up drops every sandbox ID still in the pool.
	return c.message(p.Name, 0, fmt.Sprintf("delete %d", thinID))
}

// sandboxThinID checks that table is a single sandbox thin volume, as created
// by CreateVolume, and returns its thin ID. It refuses anything else, so a
// misnamed device can never make it delete a base volume.
func (p Pool) sandboxThinID(name string, table []target) (uint32, error) {
	// "thin <pool major:minor> <thin id>"
	if len(table) == 1 && table[0].typ == "thin" {
		fields := strings.Fields(table[0].params)
		if len(fields) == 2 {
			id, err := strconv.ParseUint(fields[1], 10, 32)
			if err == nil && id >= uint64(p.FirstSandboxID) && id <= maxThinID {
				return uint32(id), nil
			}
		}
	}
	return 0, fmt.Errorf("refusing to remove %s, unexpected table: %+v", name, table)
}

// ensureNode makes path the block device dev. Device-mapper does not create
// /dev/mapper nodes, udev does, asynchronously: create it now rather than
// leave Firecracker's open() racing udev. udev may later replace it with a
// symlink to the same device, which is harmless.
func ensureNode(path string, dev uint64) error {
	want := unix.Mkdev(unix.Major(dev), unix.Minor(dev))
	for range 2 {
		var st unix.Stat_t
		err := unix.Stat(path, &st)
		if err == nil && st.Mode&unix.S_IFMT == unix.S_IFBLK && st.Rdev == want {
			return nil
		}
		if err == nil {
			// Stale node from an earlier device with this name.
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove stale node %s: %w", path, err)
			}
		}
		err = unix.Mknod(path, unix.S_IFBLK|0o600, int(want))
		if err == nil || errors.Is(err, unix.EEXIST) {
			continue // re-check: udev may have won the race
		}
		return fmt.Errorf("mknod %s: %w", path, err)
	}
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil || st.Rdev != want {
		return fmt.Errorf("node %s does not point to device %d:%d", path, unix.Major(dev), unix.Minor(dev))
	}
	return nil
}

func removeNode(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove node %s: %w", path, err)
	}
	return nil
}

// readEnvFile parses the KEY=VALUE lines of a shell-sourced settings file.
// Comments, blank lines and surrounding quotes are ignored; nothing is
// expanded.
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	vars := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		vars[strings.TrimSpace(key)] = value
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return vars, nil
}
