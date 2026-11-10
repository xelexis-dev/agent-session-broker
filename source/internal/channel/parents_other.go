//go:build !windows && !linux && !darwin

package channel

import "errors"

func processHarnessObserved(int) (string, string, error) {
	return "", "other", errors.New("parent agent inspection is supported on Windows and Linux")
}

func ancestorPIDs() ([]int, error) {
	return nil, errors.New("channel parent discovery requires Windows or Linux")
}
