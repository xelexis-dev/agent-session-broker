package serverruntime

// Opt-in real public npm clients; shares the upgrade downloader, integrity
// verifier and named Codex parent. No model or non-disposable server is used.
import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

func TestPublicAutoClientMatrix(t *testing.T) {
	assets := os.Getenv("ASB_AUTO_COMPAT_ASSETS")
	if assets == "" {
		t.Skip("opt-in: ASB_UPGRADE_HARNESS=auto-compat scripts/tests/live_upgrade_recovery.sh <asset-cache>")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("shared npm upgrade harness requires darwin/arm64")
	}
	self, err := os.Executable()
	matrixRequire(t, err == nil, "auto helper executable")
	raw, err := os.ReadFile(self)
	helper := filepath.Join(t.TempDir(), "codex")
	matrixRequire(t, err == nil && os.WriteFile(helper, raw, 0700) == nil, "auto named parent")
	for _, version := range []string{"0.16.0", "0.15.0"} {
		binary := upgradeBinary(t, assets, version, "client")
		for _, mode := range []string{"A-default", "B-unlimited", "C-gate-off", "D-manual"} {
			if !t.Run(version+"/"+mode, func(t *testing.T) { autoClientCase(t, binary, helper, version, mode) }) {
				t.FailNow()
			}
		}
	}
}

func autoClientCase(t *testing.T, binary, helper, version, mode string) {
	dir := t.TempDir()
	seed, err := controlpanel.OpenSQLite(filepath.Join(dir, "control"))
	matrixRequire(t, err == nil, "auto seed open")
	matrixRequire(t, seed.Setup("synthetic-auto-compat-password") == nil && seed.CreateProject("test") == nil, "auto seed setup")
	matrixRequire(t, seed.SetInstanceHTTPPolicy("admin", true) == nil && seed.SetScopeIssuanceReady("admin", true) == nil, "auto seed settings")
	options := controlpanel.ProjectTokenOptions{}
	if mode != "D-manual" {
		options.Scopes = append(access.LegacyV1Scopes(), access.SessionRegisterAuto)
	}
	if mode == "B-unlimited" {
		options.UnlimitedAutoBindings, options.UnlimitedAutoLifetime = true, true
	}
	parent, err := seed.IssueProjectTokenWithOptionsFor("admin", "test", "synthetic-auto-parent", options)
	matrixRequire(t, err == nil, "auto seed token")
	tokens := seed.Projects()[0].Tokens
	matrixRequire(t, len(tokens) == 1, "auto seed single parent")
	p := tokens[0]
	switch mode {
	case "A-default", "C-gate-off":
		matrixRequire(t, p.MaxAutoBindings == 8 && p.ExpiresAt.Equal(p.IssuedAt.Add(30*24*time.Hour)), "default cap and lifetime")
	case "B-unlimited":
		matrixRequire(t, p.MaxAutoBindings == 0 && p.ExpiresAt.IsZero(), "unlimited cap and lifetime")
	}
	if mode == "C-gate-off" {
		matrixRequire(t, seed.SetScopeIssuanceReady("admin", false) == nil, "auto gate off")
	}
	matrixRequire(t, seed.Close() == nil, "auto seed close")
	h, closeRuntime, err := newManagedHandler(dir, nil, "0.17.0")
	matrixRequire(t, err == nil, "auto develop runtime")
	defer closeRuntime()
	var devices, directs, approvals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/device_authorization":
			devices.Add(1)
		case "/oauth/session_registration":
			directs.Add(1)
		case "/auth/code":
			if r.Method == "POST" {
				approvals.Add(1)
			}
		}
		h.ServeHTTP(w, r)
	}))
	defer server.Close()
	count := 1
	if mode == "B-unlimited" {
		count = 9
	}
	identities, registrations := map[string]bool{}, map[string]bool{}
	for i := 0; i < count; i++ {
		home, authDir, cwd := t.TempDir(), t.TempDir(), t.TempDir()
		sid := fmt.Sprintf("11111111-1111-4111-8111-%012d", i+1)
		rollout := filepath.Join(home, ".codex", "sessions", "2026", "10", "16")
		matrixRequire(t, os.MkdirAll(rollout, 0700) == nil, "auto rollout directory")
		meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": sid, "cwd": cwd}})
		matrixRequire(t, os.WriteFile(filepath.Join(rollout, "rollout-synthetic-"+sid+".jsonl"), append(meta, '\n'), 0600) == nil, "auto rollout identity")
		c := matrixStart(t, binary, helper, server.URL+"/mcp?project=test", parent, authDir, home, cwd, sid)
		defer c.close()
		var status struct {
			Status, UserCode, Name string
			Registered             bool
		}
		manual := mode == "C-gate-off" || mode == "D-manual"
		matrixWait(t, "auto host authorization outcome", func() bool {
			return c.tool(t.Context(), "auth_status", map[string]any{}, &status) && (status.Registered || status.Status == "pending" || status.Status == "failed" || status.Status == "rejected")
		})
		if manual {
			matrixRequire(t, status.Status == "pending" && status.UserCode != "" && !status.Registered, "manual pending code, no registration")
			project, audits := autoClientEvidence(t, dir)
			matrixRequire(t, len(project.Devices) == 1 && len(project.AutoBindings) == 0, "manual persisted device, no auto binding")
			for _, e := range audits {
				matrixRequire(t, e.Kind != "grant" && e.Kind != "registration", "manual no auto grant")
			}
			matrixRequire(t, devices.Load() == 1 && approvals.Load() == 0, "manual device request without approval")
			if mode == "C-gate-off" {
				matrixRequire(t, directs.Load() == 0, "gate-off no direct exchange")
			}
			t.Logf("AUTO_COMPAT client=%s case=%s host=pending codePresent=true registered=false devices=1 autoBindings=0 approvals=0 PASS", version, mode)
			return
		}
		matrixRequire(t, status.Status == "authorized" && status.Registered && status.UserCode == "" && status.Name != "", "auto authorized without code")
		var own broker.Session
		matrixWait(t, "auto get_session ready", func() bool {
			return c.tool(t.Context(), "get_session", map[string]string{"name": status.Name}, &own) && own.AutomaticDeliveryReady
		})
		matrixRequire(t, own.RegistrationID != "" && !registrations[own.RegistrationID], "auto distinct registration")
		registrations[own.RegistrationID] = true
		project, audits := autoClientEvidence(t, dir)
		confirmed, grants, registered := 0, 0, 0
		for _, b := range project.AutoBindings {
			if b.ParentID == p.ID && b.State == "confirmed" {
				confirmed++
				identities[b.Identity] = true
			}
		}
		for _, e := range audits {
			if e.ParentID == p.ID {
				if e.Kind == "grant" {
					grants++
				}
				if e.Kind == "registration" {
					registered++
				}
			}
		}
		matrixRequire(t, confirmed == i+1 && len(identities) == i+1 && grants == i+1 && registered == i+1, "auto binding and audit counts")
		matrixRequire(t, len(project.Devices) == 0 && devices.Load() == 0 && approvals.Load() == 0, "auto zero device requests and approvals")
		for _, child := range project.Tokens {
			if child.ParentID == p.ID {
				matrixRequire(t, !child.ExpiresAt.IsZero() && child.ExpiresAt.After(time.Now()) && child.ExpiresAt.Before(time.Now().Add(31*24*time.Hour)), "auto child finite expiry")
			}
		}
		t.Logf("AUTO_COMPAT client=%s case=%s identity=%d host=authorized registered=true automaticDeliveryReady=true autoBindings=%d auditGrants=%d auditRegistrations=%d devices=0 approvals=0 PASS", version, mode, i+1, confirmed, grants, registered)
	}
}

// Read only the disposable store. Do not open another policy Store (which owns
// housekeeping); use SQLite snapshots to inspect the runtime's committed state.
func autoClientEvidence(t *testing.T, dir string) (controlpanel.Project, []controlpanel.AuditEvent) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(dir, "control"), "control.db")
	matrixRequire(t, err == nil, "auto evidence open")
	defer db.Close()
	raw, err := db.Get("state")
	var state struct {
		Projects map[string]controlpanel.Project
	}
	matrixRequire(t, err == nil && json.Unmarshal(raw, &state) == nil, "auto evidence state")
	records, err := db.List()
	matrixRequire(t, err == nil, "auto evidence audit records")
	var audits []controlpanel.AuditEvent
	for key, raw := range records {
		if strings.HasPrefix(key, "audit/") {
			var e controlpanel.AuditEvent
			matrixRequire(t, json.Unmarshal(raw, &e) == nil, "auto evidence audit decode")
			audits = append(audits, e)
		}
	}
	return state.Projects["test"], audits
}
