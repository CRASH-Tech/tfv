//go:build unix

package source

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive, cross-process advisory lock on path (creating it)
// and returns a release function. It blocks until the lock is available, so
// concurrent tfv processes operating on the same module cache run one at a time.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
