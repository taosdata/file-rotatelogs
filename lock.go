//go:build !windows
// +build !windows

package rotatelogs

import (
	"os"
	"syscall"
)

func LockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func UnlockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN|syscall.LOCK_NB)
}
