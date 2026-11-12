package controlpanel

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAPIAuditAfterRecordsTargetAndRole(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	s.apiAuditAfter("POST /api/v1/projects/{project}/members", "admin", "demo", "developer")
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.api.audits {
		if e.Action == "POST /api/v1/projects/{project}/members" && e.Actor == "admin" && e.Target == "demo" && e.Role == "developer" && e.Result == "success" {
			return
		}
	}
	t.Fatalf("audit record missing: %+v", s.api.audits)
}

func TestAPIAuditAfterDiagnosticRouteAndSuccess(t *testing.T) {
	for _, tc := range []struct{ name, route, want string }{
		{"registered", "POST /api/v1/projects/{project}/members", "POST /api/v1/projects/{project}/members"},
		{"unknown", "POST /api/v1/fixture-audit-private-input", "/api/{unmatched}"},
		{"unregistered-method", "OTHER /api/v1/projects/{project}/members", "/api/{unmatched}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := userStore(t)
			output := captureDiagnostics(t)
			s.mu.Lock()
			original := s.writeBatch
			s.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
			s.mu.Unlock()
			defer func() { s.mu.Lock(); s.writeBatch = original; s.mu.Unlock() }()
			s.apiAuditAfter(tc.route, "fixture-audit-private-input", "fixture-audit-private-input", "fixture-audit-private-input")
			text := strings.TrimSpace(output.String())
			var event map[string]any
			if err := json.Unmarshal([]byte(text), &event); err != nil {
				t.Fatal("audit diagnostic is not one JSON event")
			}
			if event["type"] != "admin-api-audit-failed" || event["route"] != tc.want || event["status"] != "success" {
				t.Error("audit diagnostic lost registered template or successful mutation status")
			}
			if strings.Contains(text, "fixture-audit-private-input") {
				t.Error("audit diagnostic exposed unregistered input or identities")
			}
		})
	}
}
