package serverruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
)

func TestExpiredGrantKeepsRegistrationAndReapprovalResumesIt(t *testing.T) {
	for _, expiry := range []string{"absolute", "idle"} {
		t.Run(expiry, func(t *testing.T) {
			root := t.TempDir()
			store, err := controlpanel.OpenSQLite(root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { store.Close() }()
			if err := store.Setup("fixture-password-long-enough"); err != nil {
				t.Fatal(err)
			}
			if err := store.CreateProject("test"); err != nil {
				t.Fatal(err)
			}
			projectToken, err := store.IssueProjectTokenFor("admin", "test", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			request := protectedRequest(1, "offline")
			approve := func(requestID string) string {
				t.Helper()
				device, err := store.BeginDeviceAuthorization("test", projectToken, controlpanel.DeviceRequest{ClientID: "asb-client-v1", Harness: request.Harness, SessionID: request.SessionID, InstanceID: request.ClientID, RequestID: requestID})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.ApproveDeviceAuthorization("admin", device.UserCode, "offline", false); err != nil {
					t.Fatal(err)
				}
				token, err := store.ExchangeDeviceToken("test", projectToken, device.DeviceCode, "asb-client-v1", false)
				if err != nil {
					t.Fatal(err)
				}
				return token.AccessToken
			}
			grantContext := func(token string) (context.Context, int) {
				t.Helper()
				var ctx context.Context
				h := controlpanel.New(store, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ctx = r.Context() }))
				req := httptest.NewRequest("GET", "https://broker/mcp?project=test", nil)
				req.Header.Set("Authorization", "Bearer "+token)
				response := httptest.NewRecorder()
				h.ServeHTTP(response, req)
				return ctx, response.Code
			}
			token := approve("approve-first")
			ctx, status := grantContext(token)
			if status != 200 {
				t.Fatal("initial authorization failed", status)
			}
			// Rebuild a context without the HTTP handler's completion cancellation.
			grant, ok := access.FromContext(ctx)
			if !ok {
				t.Fatal("no grant")
			}
			service, err := broker.NewPersistentRemoteService(filepath.Join(t.TempDir(), "registrations"))
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			backend := newProtectedBackend("test", service)
			first, err := backend.RegisterSession(access.WithGrant(context.Background(), grant), request)
			if err != nil || first.Code != "" {
				t.Fatalf("register: %s %v", first.Code, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			// Only this stopped synthetic SQLite fixture is altered.
			raw := readControlFixture(t, root)
			var state map[string]json.RawMessage
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			var projects map[string]controlpanel.Project
			if err := json.Unmarshal(state["projects"], &projects); err != nil {
				t.Fatal(err)
			}
			project := projects["test"]
			for i := range project.Tokens {
				if project.Tokens[i].Kind != "session" {
					continue
				}
				if expiry == "absolute" {
					project.Tokens[i].ExpiresAt = time.Now().Add(-time.Hour)
				} else {
					project.Tokens[i].LastUsedAt = time.Now().Add(-8 * 24 * time.Hour)
				}
			}
			projects["test"] = project
			state["projects"], err = json.Marshal(projects)
			if err != nil {
				t.Fatal(err)
			}
			raw, err = json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			writeControlFixture(t, root, raw)
			store, err = controlpanel.OpenSQLite(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, status := grantContext(token); status != 401 {
				t.Fatal("expired credential still authorizes", status)
			}
			if err := store.SweepExpired(); err != nil {
				t.Fatal(err)
			}
			listed, err := backend.ListSessions(context.Background())
			if err != nil || len(listed) != 1 || listed[0].RegistrationID != first.Session.RegistrationID {
				t.Fatalf("expiry removed registration: count=%d %v", len(listed), err)
			}
			newToken := approve("approve-second")
			newCtx, status := grantContext(newToken)
			if status != 200 {
				t.Fatal("reauthorization failed", status)
			}
			newGrant, ok := access.FromContext(newCtx)
			if !ok {
				t.Fatal("no renewed grant")
			}
			resumed, err := backend.ResumeSession(access.WithGrant(context.Background(), newGrant), broker.ResumeRequest{Holder: g4FixtureHolder(), RegistrationID: first.Session.RegistrationID, Harness: request.Harness, SessionID: request.SessionID, PID: request.PID, CWD: request.CWD, ClientID: request.ClientID, Secret: request.Secret, ChannelID: "renewed-lease", RequestID: "resume-renewed", ExpectedRevision: first.Session.Revision})
			if err != nil || resumed.Code != "" || resumed.Session.RegistrationID != first.Session.RegistrationID || resumed.Session.Name != "offline" {
				t.Fatalf("reapproval lost existing registration: %s %v", resumed.Code, err)
			}
		})
	}
}
