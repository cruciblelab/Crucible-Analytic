//go:build linux

package diskspace

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// read is Read on the platform this product is deployed to.
func read(path string) (Space, error) {
	if path == "" {
		return Space{}, fmt.Errorf("diskspace: no path given")
	}

	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return Space{}, fmt.Errorf("diskspace: %s: %w", path, err)
	}

	// f_frsize is the fragment size and is what POSIX says the block
	// counts are in; f_bsize is the preferred transfer size. They are
	// equal on every filesystem available to test this against, so the
	// preference below is untested and is here because coreutils does
	// the same - and df is what somebody will check this page against.
	unit := int64(fs.Frsize)
	if unit <= 0 {
		unit = int64(fs.Bsize)
	}
	if unit <= 0 {
		return Space{}, fmt.Errorf("diskspace: %s: the filesystem reports a block size of "+
			"zero, so none of its counts can be turned into bytes", path)
	}

	s := Space{
		Path:          path,
		TotalBytes:    blocksToBytes(fs.Blocks, unit),
		AvailBytes:    blocksToBytes(fs.Bavail, unit),
		UsedBytes:     blocksToBytes(fs.Blocks-fs.Bfree, unit),
		ReservedBytes: blocksToBytes(fs.Bfree-fs.Bavail, unit),
	}

	// The device from stat, deliberately not from statfs's f_fsid.
	//
	// f_fsid is documented as opaque, is not required to be unique, and
	// is zero for every mount on some filesystems - which would collapse
	// three real disks into one row and hide the one that is full.
	// st_dev is the identifier the kernel itself uses to tell
	// filesystems apart, and the mount check below reads it anyway.
	info, err := os.Stat(path)
	if err != nil {
		return Space{}, fmt.Errorf("diskspace: %s: %w", path, err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Space{}, fmt.Errorf("diskspace: %s: the platform does not report device "+
			"numbers", path)
	}
	s.Device = uint64(sys.Dev)

	mount, err := isMount(path, sys)
	if err != nil {
		return Space{}, err
	}
	s.Mount = mount
	return s, nil
}

// isMount reports whether path is the root of a filesystem.
//
// By comparing device numbers with the parent rather than by reading
// /proc/mounts: the parent of a mount point is on a different filesystem
// by definition, so this needs no parsing and makes no assumption about
// which of several mount tables is current.
func isMount(path string, here *syscall.Stat_t) (bool, error) {
	parent := filepath.Dir(path)
	if parent == path {
		// The root directory, whose parent is itself. It is always a
		// mount, and the comparison below would call it not one - the
		// sort of answer that is wrong in exactly one case and therefore
		// never noticed.
		return true, nil
	}
	up, err := os.Stat(parent)
	if err != nil {
		return false, fmt.Errorf("diskspace: %s: %w", parent, err)
	}
	b, ok := up.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("diskspace: %s: the platform does not report device "+
			"numbers, so whether this is a mount point cannot be determined", path)
	}
	return here.Dev != b.Dev, nil
}
