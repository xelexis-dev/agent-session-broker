package controlpanel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

type corruptPolicyBackend struct{ err error }

func (b corruptPolicyBackend) Sessions(context.Context, string) ([]Session, error) { return nil, b.err }
func (b corruptPolicyBackend) Detach(context.Context, string, string) error        { return b.err }
func (b corruptPolicyBackend) SessionSuspended(context.Context, string, string) (bool, error) {
	return false, b.err
}

func TestProjectDataErrorsExposeOnlyVerifiedCorruption(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"wrapped corruption", fmt.Errorf("private-fixture-path: %w", sqlitestore.ErrCorrupt), "project data unavailable: the server could not read this project's stored data. Ask the administrator to restore it from a backup."},
		{"ordinary IO", errors.New("private-fixture-path: IO failed"), "session policy unavailable"},
		{"untyped corruption wording", errors.New("corrupt database at private-fixture-path"), "session policy unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := userStore(t)
			token, err := s.IssueToken("demo", "reader")
			if err != nil {
				t.Fatal("token fixture failed")
			}
			h := New(s, corruptPolicyBackend{tc.err}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unavailable project reached MCP") }))
			req := httptest.NewRequest("POST", "https://broker/mcp?project=demo", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != 503 || strings.TrimSpace(out.Body.String()) != tc.want {
				t.Fatal("auth gate changed error classification or leaked private details")
			}
			admin := loginPanelUser(t, h, "admin")
			for _, page := range []string{"settings", "sessions", "audit"} {
				out = admin.call("GET", "/projects/"+page+"?project=demo", nil)
				if out.Code != 200 || strings.Contains(out.Body.String(), "private-fixture-path") {
					t.Fatal("panel leaked error details")
				}
				if strings.Contains(out.Body.String(), "project data unavailable:") {
					t.Fatal("panel used English client guidance")
				}
				has := strings.Contains(out.Body.String(), enHTML("error.project_data_unavailable"))
				if has != (tc.name == "wrapped corruption") {
					t.Errorf("%s corruption notice classification differs", page)
				}
			}
		})
	}
}
