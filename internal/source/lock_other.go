//go:build !unix

package source

import "os"

// lockFile is a best-effort no-op on platforms without flock: it just creates
// the lock file so the path exists.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}
