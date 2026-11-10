//go:build linux

package channel

import "golang.org/x/sys/unix"

func kimiSocketPeerPID(fd uintptr) int {
	peer, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0
	}
	return int(peer.Pid)
}
