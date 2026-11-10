//go:build darwin

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
		pid, inner = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if inner != nil {
			return
		}
		cred, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		inner = e
		if e == nil {
			uid = cred.Uid
		}
	})
	if err == nil {
		err = inner
	}
	return
}
