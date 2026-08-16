//go:build linux

package panel

import "syscall"

// freeBytes reports the space available to an unprivileged writer on the
// filesystem holding path.
//
// Bavail rather than Bfree: the difference is the reserve only root may
// use, and every service in this project runs as an ordinary user, so
// Bfree would report space they cannot actually have.
func freeBytes(path string) (uint64, error) {
	if path == "" {
		return 0, errNoPath
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
