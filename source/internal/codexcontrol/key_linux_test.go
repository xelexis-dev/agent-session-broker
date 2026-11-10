//go:build linux

package codexcontrol

import (
	"fmt"
	"os"
	"strings"
)

func processKeyForTest(pid int) (string, error) {
	root := fmt.Sprintf("/proc/%d", pid)
	raw, err := os.ReadFile(root + "/stat")
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return "", os.ErrNotExist
	}
	fields := strings.Fields(string(raw)[end+1:])
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return "", os.ErrNotExist
	}
	exe, err := os.Readlink(root + "/exe")
	if err != nil {
		return "", err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(boot)) + ":" + fields[19] + ":" + exe, err
}
