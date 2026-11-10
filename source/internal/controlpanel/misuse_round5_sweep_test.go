package controlpanel

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMisuse5Y1Sweep300(t *testing.T) {
	root := os.Getenv("ASB_MISUSE_Y_ROOT")
	if root == "" {
		t.Skip("explicit owned temp fixture required")
	}
	s, e := OpenSQLite(filepath.Join(root, "control"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	generation := s.ProjectGeneration("demo")
	remote, e := broker.NewSQLitePersistentRemoteServiceWithBatchCleanup(filepath.Join(root, "projects", "p-64656d6f", "registrations"), func(p []access.DetachedRemovalProof) error { return s.CleanupDetachedAutoBatch("demo", generation, p) })
	if e != nil {
		t.Fatal(e)
	}
	defer remote.Close()
	remote.SetProbeSchedule(time.Nanosecond, time.Nanosecond, 0)
	start := time.Now()
	rows, e := remote.ListSessions(t.Context())
	elapsed := time.Since(start)
	if e != nil || len(rows) != 0 {
		t.Fatal("sweep", e, len(rows))
	}
	n := 0
	for _, a := range s.audits {
		if a.Reason == "explicit-release" {
			n++
		}
	}
	if n != 200 {
		t.Fatal("sweep audit count", n)
	}
	t.Logf("sweep_ms=%.3f remaining_rows=%d explicit_release_audits=%d", float64(elapsed.Microseconds())/1000, len(rows), n)
}
func TestMisuse5Y1Inspect(t *testing.T) {
	root := os.Getenv("ASB_MISUSE_Y_ROOT")
	if root == "" {
		t.Skip("owned temp fixture required")
	}
	s, e := OpenSQLite(filepath.Join(root, "control"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	revoked, audits := 0, 0
	for _, tok := range s.data.Projects["demo"].Tokens {
		if tok.Revoked {
			revoked++
		}
	}
	for _, a := range s.audits {
		if a.Reason == "explicit-release" {
			audits++
		}
	}
	if revoked != 200 || audits != 200 || len(s.data.Projects["demo"].AutoBindings) != 0 {
		t.Fatal("listing did not clean exact grants/bindings", revoked, audits)
	}
	t.Logf("revoked=%d explicit_release_audits=%d bindings=%d", revoked, audits, len(s.data.Projects["demo"].AutoBindings))
}
