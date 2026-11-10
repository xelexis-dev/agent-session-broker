//go:build linux

package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func externalProcessKey(ctx context.Context, pid int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	root := fmt.Sprintf("/proc/%d", pid)
	executable, err := os.Readlink(filepath.Join(root, "exe"))
	if err != nil {
		return "", err
	}
	if filepath.Base(executable) != "codex" {
		return "", ErrSessionNotFound
	}
	data, err := os.ReadFile(filepath.Join(root, "stat"))
	if err != nil {
		return "", err
	}
	close := strings.LastIndexByte(string(data), ')')
	if close < 0 {
		return "", ErrSessionNotFound
	}
	fields := strings.Fields(string(data)[close+1:])
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return "", ErrSessionNotFound
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(boot)) + ":" + fields[19] + ":" + executable, nil
}
