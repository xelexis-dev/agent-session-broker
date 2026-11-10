//go:build linux

package clientstate

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func receiverProcessIdentity(ctx context.Context, pid int) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(raw) > 4096 {
		return "", errDeviceIdentity
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return "", errDeviceIdentity
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 {
		return "", errDeviceIdentity
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return "", os.ErrNotExist
	}
	if _, err = strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", errDeviceIdentity
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || len(boot) > 128 {
		return "", errDeviceIdentity
	}
	return fmt.Sprintf("%s:%s", strings.TrimSpace(string(boot)), fields[19]), nil
}
