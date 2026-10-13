package thin

import (
	"encoding/binary"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestIoctlRequestNumbers(t *testing.T) {
	// Values from linux/dm-ioctl.h on a 64-bit kernel.
	tests := map[uintptr]uintptr{
		cmdDevCreate:   0xc138fd03,
		cmdDevRemove:   0xc138fd04,
		cmdDevSuspend:  0xc138fd06,
		cmdTableLoad:   0xc138fd09,
		cmdTableStatus: 0xc138fd0c,
		cmdTargetMsg:   0xc138fd0e,
	}
	for nr, want := range tests {
		if got := ioctlRequest(nr); got != want {
			t.Errorf("ioctlRequest(%d) = %#x, want %#x", nr, got, want)
		}
	}
}

func TestNewRequestHeader(t *testing.T) {
	buf, err := newRequest("fire2-sb-x", 16)
	if err != nil {
		t.Fatal(err)
	}
	ne := binary.NativeEndian
	if len(buf) != headerSize+16 {
		t.Fatalf("len = %d", len(buf))
	}
	if v := ne.Uint32(buf[offVersion:]); v != 4 {
		t.Errorf("version major = %d, want 4", v)
	}
	if v := ne.Uint32(buf[offDataSize:]); v != headerSize+16 {
		t.Errorf("data_size = %d", v)
	}
	if v := ne.Uint32(buf[offDataStart:]); v != headerSize {
		t.Errorf("data_start = %d", v)
	}
	if name := cString(buf[offName : offName+nameSize]); name != "fire2-sb-x" {
		t.Errorf("name = %q", name)
	}

	if _, err := newRequest(string(make([]byte, nameSize)), 0); err == nil {
		t.Error("a name filling the whole field (no NUL left) must be refused")
	}
}

// Encoding then decoding a table must round-trip. Decoding reads next
// relative to data_start, so it is rewritten here the way the kernel does on
// output.
func TestTargetsRoundTrip(t *testing.T) {
	in := []target{
		{start: 0, length: 2048, typ: "thin", params: "/dev/mapper/pool 1234"},
		{start: 2048, length: 8, typ: "zero"},
	}
	payload, err := encodeTargets(in)
	if err != nil {
		t.Fatal(err)
	}
	ne := binary.NativeEndian
	if next := ne.Uint32(payload[offSpecNext:]); next%8 != 0 || int(next) < specSize+len(in[0].params)+1 {
		t.Fatalf("first spec next = %d: must be 8-aligned and cover the params", next)
	}

	// Kernel output: next is relative to data_start.
	first := ne.Uint32(payload[offSpecNext:])
	second := ne.Uint32(payload[first+offSpecNext:])
	ne.PutUint32(payload[first+offSpecNext:], first+second)

	buf, _ := newRequest("dev", len(payload))
	copy(buf[headerSize:], payload)
	ne.PutUint32(buf[offTargetCount:], uint32(len(in)))
	out, err := decodeTargets(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("decoded %d targets, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("target %d = %+v, want %+v", i, out[i], in[i])
		}
	}
}

func TestDecodeTargetsRejectsOutOfBounds(t *testing.T) {
	buf, _ := newRequest("dev", 8)
	binary.NativeEndian.PutUint32(buf[offTargetCount:], 1)
	if _, err := decodeTargets(buf); err == nil {
		t.Fatal("a spec past the end of the buffer must be refused")
	}
}

// Exercises the real ioctls with a "zero" target (no pool needed). Needs
// root and device-mapper, so it only runs on a worker host.
func TestControlRoundTrip(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	c, err := openControl()
	if err != nil {
		t.Skipf("no device-mapper: %v", err)
	}
	defer c.Close()

	const name = "fire2-test-zero"
	_ = c.removeDevice(name)
	dev, err := c.createDevice(name, []target{{start: 0, length: 2048, typ: "zero"}})
	if err != nil {
		t.Fatalf("createDevice: %v", err)
	}
	t.Cleanup(func() { _ = c.removeDevice(name) })
	if unix.Major(dev) == 0 {
		t.Fatalf("device number %#x has no major", dev)
	}

	table, err := c.table(name)
	if err != nil {
		t.Fatalf("table: %v", err)
	}
	if len(table) != 1 || table[0].typ != "zero" || table[0].length != 2048 {
		t.Fatalf("table = %+v", table)
	}

	if err := c.removeDevice(name); err != nil {
		t.Fatalf("removeDevice: %v", err)
	}
	if _, err := c.table(name); err == nil {
		t.Fatal("device still present after remove")
	}
}
