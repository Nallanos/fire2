package thin

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Device-mapper is driven through ioctls on /dev/mapper/control, the same
// interface dmsetup uses (see linux/dm-ioctl.h). Every call exchanges one
// buffer: a fixed struct dm_ioctl header, followed by a variable payload
// (target specs, a message) starting at data_start.

const controlPath = "/dev/mapper/control"

// struct dm_ioctl layout. Offsets are fixed by the kernel ABI.
const (
	headerSize = 312

	offVersion     = 0  // __u32 version[3]
	offDataSize    = 12 // __u32: size of the whole buffer, header included
	offDataStart   = 16 // __u32: where the payload starts
	offTargetCount = 20 // __u32
	offFlags       = 28 // __u32
	offDev         = 40 // __u64
	offName        = 48 // char name[128]
	nameSize       = 128
)

// struct dm_target_spec layout, followed by its NUL-terminated params.
const (
	specSize       = 40
	offSpecStart   = 0  // __u64 sector_start
	offSpecLength  = 8  // __u64 length
	offSpecNext    = 20 // __u32
	offSpecType    = 24 // char target_type[16]
	targetTypeSize = 16
)

const (
	flagStatusTable = 1 << 4 // DM_STATUS_TABLE_FLAG: table status returns the table, not the status
	flagBufferFull  = 1 << 8 // DM_BUFFER_FULL_FLAG: output did not fit, retry with a bigger buffer
)

// Ioctl commands: _IOWR(0xfd, nr, struct dm_ioctl).
const (
	cmdDevCreate   = 3
	cmdDevRemove   = 4
	cmdDevSuspend  = 6 // without DM_SUSPEND_FLAG: resume, i.e. make the loaded table live
	cmdTableLoad   = 9
	cmdTableStatus = 12
	cmdTargetMsg   = 14
)

func ioctlRequest(nr uintptr) uintptr {
	const (
		iocWrite = 1
		iocRead  = 2
		dmType   = 0xfd
	)
	return (iocRead|iocWrite)<<30 | headerSize<<16 | dmType<<8 | nr
}

// target is one line of a device-mapper table.
type target struct {
	start  uint64 // in 512-byte sectors
	length uint64
	typ    string
	params string
}

// newRequest builds a buffer with the header filled in for device name and
// room for payloadSize bytes after it.
func newRequest(name string, payloadSize int) ([]byte, error) {
	if len(name) >= nameSize {
		return nil, fmt.Errorf("device name %q too long", name)
	}
	buf := make([]byte, headerSize+payloadSize)
	ne := binary.NativeEndian
	// Interface version 4.0.0: the kernel accepts any 4.x and rejects other
	// majors.
	ne.PutUint32(buf[offVersion:], 4)
	ne.PutUint32(buf[offDataSize:], uint32(len(buf)))
	ne.PutUint32(buf[offDataStart:], headerSize)
	copy(buf[offName:offName+nameSize], name)
	return buf, nil
}

// align8 rounds n up to the 8-byte alignment target specs require.
func align8(n int) int { return (n + 7) &^ 7 }

// encodeTargets appends a table's target specs, each followed by its params.
// A spec's next field is the offset from that spec to the following one.
func encodeTargets(targets []target) ([]byte, error) {
	var out []byte
	for _, t := range targets {
		if len(t.typ) >= targetTypeSize {
			return nil, fmt.Errorf("target type %q too long", t.typ)
		}
		size := align8(specSize + len(t.params) + 1)
		spec := make([]byte, size)
		ne := binary.NativeEndian
		ne.PutUint64(spec[offSpecStart:], t.start)
		ne.PutUint64(spec[offSpecLength:], t.length)
		ne.PutUint32(spec[offSpecNext:], uint32(size))
		copy(spec[offSpecType:offSpecType+targetTypeSize], t.typ)
		copy(spec[specSize:], t.params)
		out = append(out, spec...)
	}
	return out, nil
}

// decodeTargets reads the targets a table status call wrote at data_start.
// On output, a spec's next field is relative to data_start, not to the spec.
func decodeTargets(buf []byte) ([]target, error) {
	ne := binary.NativeEndian
	if len(buf) < headerSize {
		return nil, errors.New("short dm ioctl buffer")
	}
	count := ne.Uint32(buf[offTargetCount:])
	dataStart := int(ne.Uint32(buf[offDataStart:]))
	dataSize := int(ne.Uint32(buf[offDataSize:]))
	if dataSize > len(buf) || dataStart > dataSize {
		return nil, errors.New("dm ioctl output out of bounds")
	}
	data := buf[dataStart:dataSize]

	targets := make([]target, 0, count)
	off := 0
	for range count {
		if off+specSize > len(data) {
			return nil, errors.New("dm target spec out of bounds")
		}
		spec := data[off:]
		params := spec[specSize:]
		if end := bytes.IndexByte(params, 0); end >= 0 {
			params = params[:end]
		}
		targets = append(targets, target{
			start:  ne.Uint64(spec[offSpecStart:]),
			length: ne.Uint64(spec[offSpecLength:]),
			typ:    cString(spec[offSpecType : offSpecType+targetTypeSize]),
			params: string(params),
		})
		off = int(ne.Uint32(spec[offSpecNext:]))
	}
	return targets, nil
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// control is an open /dev/mapper/control.
type control struct {
	f *os.File
}

func openControl() (*control, error) {
	f, err := os.OpenFile(controlPath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", controlPath, err)
	}
	return &control{f: f}, nil
}

func (c *control) Close() error { return c.f.Close() }

func (c *control) ioctl(nr uintptr, buf []byte) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, c.f.Fd(), ioctlRequest(nr), uintptr(unsafe.Pointer(&buf[0])))
	// The kernel reads and writes buf through a uintptr the GC cannot see.
	runtime.KeepAlive(buf)
	if errno != 0 {
		return errno
	}
	return nil
}

// createDevice creates device name with a table and makes it live, the way
// `dmsetup create --table` does: create, load the table, resume. It returns
// the device number. On failure, nothing is left behind.
func (c *control) createDevice(name string, targets []target) (dev uint64, err error) {
	buf, err := newRequest(name, 0)
	if err != nil {
		return 0, err
	}
	if err := c.ioctl(cmdDevCreate, buf); err != nil {
		return 0, fmt.Errorf("create device %s: %w", name, err)
	}
	dev = binary.NativeEndian.Uint64(buf[offDev:])
	defer func() {
		if err != nil {
			if rmErr := c.removeDevice(name); rmErr != nil {
				err = errors.Join(err, rmErr)
			}
		}
	}()

	payload, err := encodeTargets(targets)
	if err != nil {
		return 0, err
	}
	if buf, err = newRequest(name, len(payload)); err != nil {
		return 0, err
	}
	copy(buf[headerSize:], payload)
	binary.NativeEndian.PutUint32(buf[offTargetCount:], uint32(len(targets)))
	if err := c.ioctl(cmdTableLoad, buf); err != nil {
		return 0, fmt.Errorf("load table of %s: %w", name, err)
	}

	if buf, err = newRequest(name, 0); err != nil {
		return 0, err
	}
	if err := c.ioctl(cmdDevSuspend, buf); err != nil {
		return 0, fmt.Errorf("resume %s: %w", name, err)
	}
	return dev, nil
}

// removeRetries and removeRetryDelay bound how long removeDevice waits for a
// device to be closed (e.g. the Firecracker process that held it is still
// exiting), like dmsetup remove --retry.
const (
	removeRetries    = 25
	removeRetryDelay = 200 * time.Millisecond
)

// removeDevice removes device name. ENXIO means it does not exist.
func (c *control) removeDevice(name string) error {
	for attempt := 0; ; attempt++ {
		buf, err := newRequest(name, 0)
		if err != nil {
			return err
		}
		err = c.ioctl(cmdDevRemove, buf)
		if errors.Is(err, unix.EBUSY) && attempt < removeRetries {
			time.Sleep(removeRetryDelay)
			continue
		}
		if err != nil {
			return fmt.Errorf("remove device %s: %w", name, err)
		}
		return nil
	}
}

// table returns device name's live table. ENXIO means it does not exist.
func (c *control) table(name string) ([]target, error) {
	for size := 4096; ; size *= 2 {
		buf, err := newRequest(name, size)
		if err != nil {
			return nil, err
		}
		binary.NativeEndian.PutUint32(buf[offFlags:], flagStatusTable)
		if err := c.ioctl(cmdTableStatus, buf); err != nil {
			return nil, fmt.Errorf("table of %s: %w", name, err)
		}
		if binary.NativeEndian.Uint32(buf[offFlags:])&flagBufferFull != 0 {
			if size >= 1<<20 {
				return nil, fmt.Errorf("table of %s: too large", name)
			}
			continue
		}
		return decodeTargets(buf)
	}
}

// message sends a target message to device name, like `dmsetup message`.
func (c *control) message(name string, sector uint64, msg string) error {
	// struct dm_target_msg: __u64 sector, then the NUL-terminated message.
	buf, err := newRequest(name, align8(8+len(msg)+1))
	if err != nil {
		return err
	}
	binary.NativeEndian.PutUint64(buf[headerSize:], sector)
	copy(buf[headerSize+8:], msg)
	if err := c.ioctl(cmdTargetMsg, buf); err != nil {
		return fmt.Errorf("message %q to %s: %w", msg, name, err)
	}
	return nil
}
