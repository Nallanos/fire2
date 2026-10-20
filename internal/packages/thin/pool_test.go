package thin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadPoolAndBase(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "thinpool.conf")
	writeFile(t, conf, `# Managed by Ansible
POOL_NAME=fire2-pool
POOL_DIR=`+dir+`
DATA_SIZE=20G

FIRST_SANDBOX_ID=1000
`)
	p, err := LoadPool(conf)
	if err != nil {
		t.Fatalf("LoadPool: %v", err)
	}
	if p.Name != "fire2-pool" || p.Dir != dir || p.FirstSandboxID != 1000 {
		t.Fatalf("pool = %+v", p)
	}

	if _, err := p.loadBase(); err == nil {
		t.Fatal("loadBase without base.env must fail")
	}
	writeFile(t, filepath.Join(dir, "base.env"), "BASE_THIN_ID=3\nBASE_SECTORS=2097152\nBASE_SHA256=abc\n")
	b, err := p.loadBase()
	if err != nil {
		t.Fatalf("loadBase: %v", err)
	}
	if b.thinID != 3 || b.sectors != 2097152 {
		t.Fatalf("base = %+v", b)
	}

	// A base ID in the sandbox range would make removals able to delete it.
	writeFile(t, filepath.Join(dir, "base.env"), "BASE_THIN_ID=1000\nBASE_SECTORS=8\n")
	if _, err := p.loadBase(); err == nil {
		t.Fatal("a base thin ID in the sandbox range must be refused")
	}
}

func TestLoadPoolRequiresSettings(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "thinpool.conf")
	writeFile(t, conf, "POOL_NAME=p\nPOOL_DIR=/x\n")
	if _, err := LoadPool(conf); err == nil {
		t.Fatal("missing FIRST_SANDBOX_ID must be refused")
	}
}

func TestReadEnvFileStripsQuotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.env")
	writeFile(t, path, "A=\"one\"\nB='two'\nC=three # not a comment\n")
	vars, err := readEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if vars["A"] != "one" || vars["B"] != "two" || vars["C"] != "three # not a comment" {
		t.Fatalf("vars = %v", vars)
	}
}

func TestVolumeNameValidation(t *testing.T) {
	for _, id := range []string{"6f1c2a52-58a4-4f4e-9a5f-0d1d2f6b7c01", "smoke-abc_1"} {
		if _, err := volumeName(id); err != nil {
			t.Errorf("volumeName(%q): %v", id, err)
		}
	}
	for _, id := range []string{"", "../etc", "a b", strings.Repeat("a", 65)} {
		if _, err := volumeName(id); err == nil {
			t.Errorf("volumeName(%q) must fail", id)
		}
	}
}

func TestSandboxThinIDRefusesNonSandboxTables(t *testing.T) {
	p := Pool{Name: "pool", FirstSandboxID: 1000}
	ok := []target{{length: 8, typ: "thin", params: "253:0 4242"}}
	if id, err := p.sandboxThinID("v", ok); err != nil || id != 4242 {
		t.Fatalf("sandboxThinID = %d, %v", id, err)
	}
	for _, table := range [][]target{
		{{typ: "thin", params: "253:0 3"}},                   // base volume
		{{typ: "linear", params: "8:0 0"}},                   // not thin
		{{typ: "thin", params: "253:0 4242"}, {typ: "zero"}}, // two targets
		{{typ: "thin", params: "253:0"}},                     // no ID
	} {
		if _, err := p.sandboxThinID("v", table); err == nil {
			t.Errorf("table %+v must be refused", table)
		}
	}
}

// Runs CreateVolume/RemoveVolume against a real pool, described by the
// thinpool.conf at $FIRE2_TEST_THINPOOL_CONF (with a base.env next to the
// pool). Needs root: run it on a worker host or in a privileged container.
func TestPoolCreateRemove(t *testing.T) {
	confPath := os.Getenv("FIRE2_TEST_THINPOOL_CONF")
	if confPath == "" {
		t.Skip("FIRE2_TEST_THINPOOL_CONF not set")
	}
	p, err := LoadPool(confPath)
	if err != nil {
		t.Fatalf("LoadPool: %v", err)
	}

	ids := []string{"test-a", "test-b", "test-c"}
	for _, id := range ids {
		path, err := p.CreateVolume(id)
		if err != nil {
			t.Fatalf("CreateVolume(%s): %v", id, err)
		}
		st, err := os.Stat(path)
		if err != nil || st.Mode()&os.ModeDevice == 0 {
			t.Fatalf("%s is not a device node: %v", path, err)
		}
		// The snapshot must be readable end to end, like Firecracker will.
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		if _, err := f.Read(make([]byte, 4096)); err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		f.Close()
	}

	if _, err := p.CreateVolume("test-a"); err == nil {
		t.Fatal("creating an existing volume must fail")
	}

	c, err := openControl()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	table, err := c.table(volumePrefix + "test-a")
	if err != nil {
		t.Fatalf("table: %v", err)
	}
	thinID, err := p.sandboxThinID("test-a", table)
	if err != nil {
		t.Fatalf("sandboxThinID: %v", err)
	}

	for _, id := range ids {
		if err := p.RemoveVolume(id); err != nil {
			t.Fatalf("RemoveVolume(%s): %v", id, err)
		}
		if _, err := os.Stat(VolumePath(id)); !os.IsNotExist(err) {
			t.Fatalf("node of %s still present: %v", id, err)
		}
		if err := p.RemoveVolume(id); err != nil {
			t.Fatalf("RemoveVolume(%s) twice must be a no-op: %v", id, err)
		}
	}

	// The thin ID was freed in the pool: it can be allocated again.
	if err := c.message(p.Name, 0, fmt.Sprintf("create_thin %d", thinID)); err != nil {
		t.Fatalf("thin ID %d still allocated after remove: %v", thinID, err)
	}
	if err := c.message(p.Name, 0, fmt.Sprintf("delete %d", thinID)); err != nil {
		t.Fatal(err)
	}
}
