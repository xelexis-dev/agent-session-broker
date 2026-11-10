//go:build darwin || linux

package channel

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

func kimiPrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || int(stat.Uid) != os.Getuid() {
		return errors.New("unsafe Kimi socket directory")
	}
	return nil
}
func kimiSocketPath(state string, pid int) (string, error) {
	if !filepath.IsAbs(state) {
		return "", errors.New("Kimi state directory must be absolute")
	}
	dir := filepath.Join(state, "kimi")
	if len(filepath.Join(dir, kimiSocketName(pid))) >= 104 {
		base, err := filepath.EvalSymlinks("/tmp")
		if err != nil {
			return "", err
		}
		// Scope by resolved state directory so distinct broker profiles cannot collide.
		real, err := filepath.EvalSymlinks(state)
		if err != nil {
			return "", err
		}
		hash := sha256.Sum256([]byte(real))
		dir = filepath.Join(base, fmt.Sprintf("asb-kimi-%d-%x", os.Getuid(), hash[:8]))
	}
	if err := kimiPrivateDir(dir); err != nil {
		return "", err
	}
	return filepath.Join(dir, kimiSocketName(pid)), nil
}
func kimiPeerPID(conn *net.UnixConn) int {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0
	}
	pid := 0
	err = raw.Control(func(fd uintptr) { pid = kimiSocketPeerPID(fd) })
	if err != nil {
		return 0
	}
	return pid
}
