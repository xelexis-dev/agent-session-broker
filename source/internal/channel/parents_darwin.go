//go:build darwin

package channel

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/processinfo"
	"os"
)

func processHarnessObserved(pid int) (string, string, error) {
	info, err := processinfo.Read(pid)
	if err != nil {
		return "", "other", parentFailure("query", err)
	}
	return harnessFromProcess(info.Executable, info.EntryScript, info.Title), parentImageLabel(info.Executable), nil
}

func ancestorPIDs() ([]int, error) {
	pid := os.Getppid()
	var result []int
	for i := 0; i < 16 && pid > 0; i++ {
		result = append(result, pid)
		next, err := processinfo.Parent(pid)
		if err != nil {
			break
		}
		if next == pid {
			break
		}
		pid = next
	}
	return result, nil
}
