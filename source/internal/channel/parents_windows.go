//go:build windows

package channel

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func processHarnessObserved(pid int) (string, string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", "other", parentFailure("open", err)
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	// The native name stays stable when the running executable is renamed by its updater.
	if err := windows.QueryFullProcessImageName(h, windows.PROCESS_NAME_NATIVE, &buf[0], &size); err != nil {
		return "", "other", parentFailure("query", err)
	}
	image := windows.UTF16ToString(buf[:size])
	return harnessFromWindowsImage(image), parentImageLabel(image), nil
}

func ancestorPIDs() ([]int, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, parentFailure("snapshot", err)
	}
	defer windows.CloseHandle(snapshot)
	return windowsAncestorPIDs(uint32(os.Getpid()), func(e *windows.ProcessEntry32) error { return windows.Process32First(snapshot, e) }, func(e *windows.ProcessEntry32) error { return windows.Process32Next(snapshot, e) })
}

func windowsAncestorPIDs(self uint32, first, next func(*windows.ProcessEntry32) error) ([]int, error) {
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	parents := map[uint32]uint32{}
	err := first(&entry)
	if err != nil && !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, parentFailure("first", err)
	}
	for err == nil {
		parents[entry.ProcessID] = entry.ParentProcessID
		err = next(&entry)
		if err != nil && !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return nil, parentFailure("next", err)
		}
	}
	if _, ok := parents[self]; !ok {
		return nil, parentFailure("selfMissing", nil)
	}
	var result []int
	pid := self
	seen := map[uint32]bool{self: true}
	for i := 0; i < 16; i++ {
		pid = parents[pid]
		if pid == 0 {
			break
		}
		if seen[pid] {
			return nil, parentFailure("cycle", nil)
		}
		seen[pid] = true
		result = append(result, int(pid))
	}
	return result, nil
}
