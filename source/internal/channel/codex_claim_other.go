//go:build !darwin && !linux

package channel

import (
	"os"
	"time"
)

func openCodexClaim(string) (*os.File, error)       { return nil, os.ErrNotExist }
func codexClaimProcessStart(int) (time.Time, error) { return time.Time{}, os.ErrNotExist }
