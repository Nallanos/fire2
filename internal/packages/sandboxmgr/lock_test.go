package sandboxmgr

import (
	"errors"
	"testing"
)

func TestLockBaseDirIsExclusive(t *testing.T) {
	dir := t.TempDir()

	first, err := LockBaseDir(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	// flock locks belong to the open file description, so a second open in
	// the same process conflicts exactly like a second worker process would.
	if _, err := LockBaseDir(dir); !errors.Is(err, ErrBaseDirLocked) {
		t.Fatalf("second lock = %v, want ErrBaseDirLocked", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("release: %v", err)
	}
	second, err := LockBaseDir(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	second.Close()
}
