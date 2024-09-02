//go:build openbsd
// +build openbsd

package rotatelogs

import "golang.org/x/sys/unix"

func GetDiskSize(dir string) (total uint64, avail uint64, err error) {
	fs := unix.Statfs_t{}
	err = unix.Statfs(dir, &fs)
	if err != nil {
		return 0, 0, err
	}
	avail = uint64(fs.F_bavail) * uint64(fs.F_bsize)
	total = fs.F_blocks * uint64(fs.F_bsize)
	return total, avail, nil
}
