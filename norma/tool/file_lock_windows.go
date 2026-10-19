//go:build windows

package tool

import (
	"os"
	"syscall"
	"unsafe"
)

var lockFileProc = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
var unlockFileProc = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")

func tryFileLock(f *os.File) (bool, error) {
	var ov syscall.Overlapped
	r, _, err := lockFileProc.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&ov)))
	if r != 0 {
		return true, nil
	}
	if err == syscall.Errno(33) {
		return false, nil
	}
	return false, err
}
func unlockFile(f *os.File) {
	var ov syscall.Overlapped
	_, _, _ = unlockFileProc.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ov)))
}
