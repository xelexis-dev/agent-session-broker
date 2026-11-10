//go:build !darwin && !linux

package channel

import (
	"errors"
	"net"
)

func kimiSocketPath(string, int) (string, error) {
	return "", errors.New("Kimi hooks require macOS or Linux")
}
func kimiPeerPID(*net.UnixConn) int { return 0 }
