package sandboxmgr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockFileName lives in the sandbox base dir. It is not a UUID, so the
// reconciliation never mistakes it for a sandbox.
const lockFileName = ".fire2-worker.lock"

// ErrBaseDirLocked is returned by LockBaseDir when another process already
// owns the sandbox base dir.
var ErrBaseDirLocked = errors.New("sandbox base dir is locked by another worker")

// LockBaseDir takes an exclusive lock on the sandbox base dir, so a single
// worker process owns it: every worker reaps whatever it does not track, and
// two of them on the same dir would reap each other's sandboxes. It fails
// immediately if another process holds the lock.
//
// The lock is held as long as the returned file stays open. The kernel drops
// it when the process dies, crash included, so there is no stale lock to
// clean up. Keep the file referenced for the process lifetime.
func LockBaseDir(baseDir string) (*os.File, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create sandbox base dir: %w", err)
	}
	path := filepath.Join(baseDir, lockFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrBaseDirLocked, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}
