//go:build linux

package localactivity

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
)

func nativeProcess(ctx context.Context, pid int) (Process, error) {
	if ctx.Err() != nil || pid <= 0 {
		return Process{}, unavailable
	}
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return Process{}, unavailable
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 {
		return Process{}, unavailable
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return Process{}, unavailable
	}
	// Skip the comm field without retaining it. Field 22 is the kernel's
	// process creation tick count; no cmdline or environment is accessed.
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return Process{}, unavailable
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return Process{}, unavailable
	}
	host, err := os.Hostname()
	if err != nil {
		return Process{}, unavailable
	}
	return Process{Key: "linux:" + fields[19], Host: host, Platform: "linux"}, nil
}
