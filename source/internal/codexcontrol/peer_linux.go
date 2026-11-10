//go:build linux

package codexcontrol

import (
	"golang.org/x/sys/unix"
	"net"
)

func peerIdentity(conn *net.UnixConn) (pid int, uid uint32, err error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var inner error
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		inner = e
		if e == nil {
			pid = int(cred.Pid)
			uid = cred.Uid
		}
	})
	if err == nil {
		err = inner
	}
	return
}
