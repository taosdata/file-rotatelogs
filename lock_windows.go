//go:build windows
// +build windows

package rotatelogs

import (
	"os"

	"golang.org/x/sys/windows"
)

func LockFile(file *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(file.Fd()), uint32(windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY), 0, ^uint32(0), ^uint32(0), ol)
}

func UnlockFile(file *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, ^uint32(0), ^uint32(0), ol)
}
