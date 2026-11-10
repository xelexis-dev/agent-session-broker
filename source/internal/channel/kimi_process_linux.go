//go:build linux

package channel

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func kimiProcessKey(pid int) (string, error) {
	root := fmt.Sprintf("/proc/%d", pid)
	data, e := os.ReadFile(root + "/stat")
	if e != nil {
		return "", e
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", os.ErrNotExist
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return "", os.ErrNotExist
	}
	exe, e := os.Readlink(root + "/exe")
	if e != nil {
		return "", e
	}
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return "", e
	}
	return strings.TrimSpace(string(boot)) + ":" + fields[19] + ":" + exe, nil
}
func kimiParentPID(pid int) (int, error) {
	data, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e != nil {
		return 0, e
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, os.ErrNotExist
	}
	f := strings.Fields(string(data)[end+1:])
	if len(f) < 2 {
		return 0, os.ErrNotExist
	}
	return strconv.Atoi(f[1])
}
