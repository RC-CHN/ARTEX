package runnerfiles

import (
	"os"
	"syscall"
)

func singleLink(f *os.File, _ os.FileInfo) bool {
	var info syscall.ByHandleFileInformation
	return syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info) == nil && info.NumberOfLinks == 1
}
