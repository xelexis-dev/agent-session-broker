package nativeinbox

import "syscall"

func connectionFromParent(conn writeConnection, expectedPID int) bool {
	c, ok := conn.(syscall.Conn)
	if !ok {
		return false
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return false
	}
	pid := 0
	err = raw.Control(func(fd uintptr) { pid = socketPeerPID(fd) })
	return err == nil && pid == expectedPID
}
