package nativeinbox

import "golang.org/x/sys/windows"

func socketPeerPID(fd uintptr) int {
	var pid uint32
	if windows.GetNamedPipeServerProcessId(windows.Handle(fd), &pid) != nil {
		return 0
	}
	return int(pid)
}
