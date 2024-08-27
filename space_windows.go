//go:build windows
// +build windows

package rotatelogs

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func GetDiskSize(dir string) (total uint64, avail uint64, err error) {
	d, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, fmt.Errorf(`failed to call UTF16PtrFromString, d:%s, err: %s`, dir, err)
	}
	var freeBytesAvailableToCaller, totalNumberOfBytes, totalNumberOfFreeBytes uint64
	err = windows.GetDiskFreeSpaceEx(d, &freeBytesAvailableToCaller, &totalNumberOfBytes, &totalNumberOfFreeBytes)
	if err != nil {
		return 0, 0, err
	}
	return totalNumberOfBytes, freeBytesAvailableToCaller, nil
}
