package controlpanel

import "testing"

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
