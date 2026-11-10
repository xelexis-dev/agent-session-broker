//go:build (!windows && !linux && !darwin) || android

package pathguard

import (
	"os"
	"runtime"
)

func NewPin(_ string, _ string, _ os.FileInfo, labels PinLabels) (Pin, error) {
	return unsupportedPin(runtime.GOOS, labels)
}
