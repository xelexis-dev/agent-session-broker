//go:build darwin

// Package processinfo reads macOS process identity without spawning a shell.
package processinfo

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type Info struct {
	Parent      int
	Executable  string
	EntryScript string
	Title       string
	Key         string
}

func Read(pid int) (Info, error) {
	if pid <= 0 {
		return Info{}, os.ErrNotExist
	}
	before, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return Info{}, err
	}
	if before.Proc.P_pid != int32(pid) || before.Proc.P_stat == 0 || before.Proc.P_stat == 5 {
		return Info{}, os.ErrNotExist
	}
	args, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return Info{}, err
	}
	if len(args) <= 4 {
		return Info{}, os.ErrNotExist
	}
	end := bytes.IndexByte(args[4:], 0)
	if end <= 0 {
		return Info{}, os.ErrNotExist
	}
	executable := string(args[4 : 4+end])
	if !filepath.IsAbs(executable) {
		return Info{}, os.ErrNotExist
	}
	// Read only Node title/entry fields; never expose remaining argv or environment.
	var entry, title string
	if filepath.Base(executable) == "node" && binary.NativeEndian.Uint32(args[:4]) >= 1 {
		tail := bytes.TrimLeft(args[4+end+1:], "\x00")
		if first := bytes.IndexByte(tail, 0); first >= 0 {
			title = string(tail[:first])
			if binary.NativeEndian.Uint32(args[:4]) >= 2 {
				tail = tail[first+1:]
				if second := bytes.IndexByte(tail, 0); second >= 0 {
					entry = string(tail[:second])
				}
			}
		}
	}
	after, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return Info{}, err
	}
	if after.Proc.P_pid != int32(pid) || after.Proc.P_stat == 0 || after.Proc.P_stat == 5 || after.Proc.P_starttime != before.Proc.P_starttime {
		return Info{}, os.ErrNotExist
	}
	started := before.Proc.P_starttime
	return Info{Parent: int(before.Eproc.Ppid), Executable: executable, EntryScript: entry, Title: title, Key: fmt.Sprintf("darwin:%d:%d:%s", started.Sec, started.Usec, executable)}, nil
}

func Parent(pid int) (int, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	if info.Proc.P_pid != int32(pid) {
		return 0, os.ErrNotExist
	}
	return int(info.Eproc.Ppid), nil
}
