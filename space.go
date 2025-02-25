//go:build linux && !s390x && !netbsd
// +build linux,!s390x,!netbsd

package rotatelogs

import (
	"golang.org/x/sys/unix"
)

func GetDiskSize(dir string) (total uint64, avail uint64, err error) {
	fs := unix.Statfs_t{}
	err = unix.Statfs(dir, &fs)
	if err != nil {
		return 0, 0, err
	}
	avail = uint64(fs.Bavail) * uint64(fs.Frsize)
	total = uint64(fs.Blocks) * uint64(fs.Frsize)
	return total, avail, nil
}
