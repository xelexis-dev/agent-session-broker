//go:build linux

package channel

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func processHarnessObserved(pid int) (string, string, error) {
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return "", "other", parentFailure("query", err)
	}
	if filepath.Base(exe) != "node" {
		return harnessFromExecutable(exe), parentImageLabel(exe), nil
	}
	before, err := kimiProcessKey(pid)
	if err != nil {
		return "", "other", parentFailure("query", err)
	}
	f, err := os.Open(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", "other", parentFailure("query", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return "", "other", parentFailure("query", err)
	}
	argv := bytes.Split(data, []byte{0})
	entry := ""
	if len(argv) > 1 {
		entry = string(argv[1])
	}
	if entry != "" && !filepath.IsAbs(entry) {
		cwd, e := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		if e != nil {
			return "", "other", parentFailure("query", e)
		}
		entry = filepath.Join(cwd, entry)
	}
	after, err := kimiProcessKey(pid)
	if err != nil || after != before {
		return "", "other", parentFailure("query", os.ErrNotExist)
	}
	return harnessFromProcess(exe, entry, string(argv[0])), parentImageLabel(exe), nil
}

func ancestorPIDs() ([]int, error) {
	pid := os.Getppid()
	var result []int
	for i := 0; i < 16 && pid > 0; i++ {
		result = append(result, pid)
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			break
		}
		next := 0
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "PPid:") {
				next, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
				break
			}
		}
		pid = next
	}
	return result, nil
}
