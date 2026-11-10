//go:build darwin || linux

package channel

import (
	"golang.org/x/sys/unix"
	"os"
	"testing"
)

func TestCodexClaimForeignOwnerIsRejected(t *testing.T) {
	st := unix.Stat_t{Uid: uint32(os.Getuid() + 1), Mode: unix.S_IFREG | 0600, Size: 10}
	if safeCodexClaimStat(&st) {
		t.Fatal("foreign uid accepted")
	}
	st.Uid = uint32(os.Getuid())
	if !safeCodexClaimStat(&st) {
		t.Fatal("private regular file refused")
	}
}
