//go:build !darwin && !linux

package channel

import (
	"os"
	"time"
)

func publishCodexYield(string, []byte) error                 { return os.ErrNotExist }
func readCodexYield(string, bool) ([]byte, time.Time, error) { return nil, time.Time{}, os.ErrNotExist }
func removeCodexYield(string)                                {}
