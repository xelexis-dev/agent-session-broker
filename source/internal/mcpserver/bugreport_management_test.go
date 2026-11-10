package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
)

// Decode all three wire outputs for shared assertions.
type bugReportManagementOutput struct {
	bugReportProblem
	Reports         []bugReportSummary `json:"reports"`
	Total           int                `json:"total"`
	Page            int                `json:"page"`
	Report          *bugReportDetail   `json:"report"`
	ID              string             `json:"id"`
	State           string             `json:"state"`
	ExpectedVersion string             `json:"expectedVersion"`
	Changed         bool               `json:"changed"`
	StatusChanges   []bugReportChange  `json:"statusChanges"`
}

func managementFixture(t *testing.T, mode string) (*mcp.ClientSession, *bugreport.Store, bugreport.Report) {
	t.Helper()
	remote := broker.NewRemoteService()
	t.Cleanup(func() { remote.Close() })
	req := broker.RegisterRequest{Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", PID: 123, CWD: "/synthetic", ClientID: "fixture-pc", Name: "worker", ChannelID: "fixture-lease", Secret: strings.Repeat("s", 40), RequestID: "fixture-request"}
	result, err := remote.RegisterSession(t.Context(), req)
	if err != nil || result.Code != "" {
		t.Fatal(err, result.Code)
	}
	store, err := bugreport.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	first := "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	second := "BBBBBBBBBBBBBBBBBBBBBBBBBB"
	// The two raw names redact identically; identity must remain the generation ID.
	project := "project-" + strings.Join([]string{"asb", "p", "one"}, "_")
	otherProject := "project-" + strings.Join([]string{"asb", "p", "two"}, "_")
	r, err := store.Create(bugreport.Input{Title: "bug", Summary: "untrusted body", Client: &bugreport.ClientDiagnostics{ClientVersion: "fixture"}}, bugreport.Reporter{ProjectGenerationID: first, Project: project, SessionName: "worker", RegistrationID: "reg", Harness: "codex", OwnerID: "private-owner-id"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	store.SetProjectPolicy(func(actor string) (map[string]string, func(), error) {
		if actor != "" && actor != "admin-id" {
			return nil, nil, bugreport.ErrForbidden
		}
		current := map[string]string{project: first, otherProject: second}
		if mode == "recreated" || mode == "recreated-current" {
			current[project] = second
		}
		return current, func() {}, nil
	})
	revoked := make(chan struct{})
	grant := access.Grant{ProjectGenerationID: first, TokenID: "fixture", Project: project, OwnerID: "admin-id", Scopes: []string{access.SessionManage}, ExpiresAt: time.Now().Add(time.Hour), Revoked: revoked, Check: func(identity string) error {
		var fields [3]string
		if json.Unmarshal([]byte(identity), &fields) != nil || fields != [3]string{req.Harness, req.SessionID, req.ClientID} || mode == "unregistered" {
			return errors.New("refused")
		}
		return nil
	}}
	switch mode {
	case "nonadmin":
		grant.OwnerID = "ordinary-user"
	case "no-scope":
		grant.Scopes = nil
	case "expired":
		grant.ExpiresAt = time.Now().Add(-time.Hour)
	case "revoked":
		close(revoked)
	case "other-project":
		grant.Project = otherProject
		grant.ProjectGenerationID = second
	case "recreated-current":
		grant.ProjectGenerationID = second
	case "previous-generation":
		grant.ProjectGenerationID = "CCCCCCCCCCCCCCCCCCCCCCCCCC"
	case "suspended":
		if err = remote.SetSuspended(t.Context(), result.Session.Name, true); err != nil {
			t.Fatal(err)
		}
	case "detached":
		if err = remote.StopSession(t.Context(), result.Session.Name); err != nil {
			t.Fatal(err)
		}
	}
	server := New(remote, &stubMemory{}, "0.17.3")
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			ctx = bugreport.WithStore(ctx, store)
			ctx = bugreport.WithActorName(ctx, func(string) string { return "Administrator" })
			if mode != "no-grant" {
				ctx = access.WithGrant(ctx, grant)
			}
			return next(ctx, method, request)
		}
	})
	return catalogSession(t, server), store, r
}
func managementCall(t *testing.T, c *mcp.ClientSession, name string, args any) (*mcp.CallToolResult, bugReportManagementOutput) {
	t.Helper()
	r, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(r.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out bugReportManagementOutput
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return r, out
}
func TestBugReportManagementAuthorization(t *testing.T) {
	for _, mode := range []string{"valid", "nonadmin", "unregistered", "suspended", "detached", "expired", "revoked", "no-grant", "no-scope", "other-project", "recreated", "recreated-current", "previous-generation"} {
		t.Run(mode, func(t *testing.T) {
			c, store, r := managementFixture(t, mode)
			for _, name := range []string{"list_bug_reports", "get_bug_report", "set_bug_report_status"} {
				var args any = bugReportListInput{State: "all"}
				if name == "get_bug_report" {
					args = bugReportGetInput{ID: r.ID}
				}
				if name == "set_bug_report_status" {
					args = bugReportStatusInput{ID: r.ID, State: "resolved", ExpectedVersion: r.ExpectedVersion, Note: "fixed"}
				}
				result, out := managementCall(t, c, name, args)
				if mode == "valid" {
					if result.IsError {
						t.Fatal(name, out)
					}
					continue
				}
				if (mode == "other-project" || mode == "recreated-current") && name == "list_bug_reports" {
					if result.IsError || out.Total != 0 {
						t.Fatal("other scope list")
					}
					continue
				}
				want := "forbidden"
				if mode == "other-project" || mode == "recreated-current" {
					want = "not-found"
				}
				if !result.IsError || out.Code != want {
					t.Fatalf("%s: got %+v error=%v, want %s", name, out, result.IsError, want)
				}
				raw, _ := json.Marshal(out)
				if strings.Contains(string(raw), r.ID) || strings.Contains(string(raw), "untrusted body") {
					t.Fatal("denial leaked data")
				}
			}
			if mode != "valid" {
				got, err := store.GetFor("admin-id", r.ID, "history")
				if mode != "recreated" && mode != "recreated-current" {
					got, err = store.GetFor("admin-id", r.ID, "current")
				}
				if err != nil || got.Status != "open" {
					t.Fatal("denial mutated report", err)
				}
			}
		})
	}
}
func TestBugReportManagementOutputsAndErrors(t *testing.T) {
	c, _, r := managementFixture(t, "valid")
	listed, err := c.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"list_bug_reports", "get_bug_report", "set_bug_report_status"} {
		if toolScope(name) != access.SessionManage {
			t.Fatal("tool scope", name)
		}
		found := false
		for _, tool := range listed.Tools {
			if tool.Name == name {
				found = true
				if !strings.Contains(tool.Description, "untrusted") {
					t.Fatal("missing trust warning")
				}
			}
		}
		if !found {
			t.Fatal("missing tool", name)
		}
	}
	result, out := managementCall(t, c, "list_bug_reports", bugReportListInput{})
	raw, _ := json.Marshal(result.StructuredContent)
	if result.IsError || len(out.Reports) != 1 || out.Reports[0].ExpectedVersion == "" || strings.Contains(string(raw), "summary") || strings.Contains(string(raw), "client") || strings.Contains(string(raw), "private-owner-id") {
		t.Fatal("summary exposure", string(raw))
	}
	result, out = managementCall(t, c, "set_bug_report_status", bugReportStatusInput{ID: r.ID, State: "resolved", ExpectedVersion: r.ExpectedVersion, Note: "fixed"})
	if result.IsError || !out.Changed || out.State != "resolved" || len(out.StatusChanges) != 1 || out.StatusChanges[0].Session == nil || out.StatusChanges[0].Actor != "Administrator" {
		t.Fatal("mutation output", out)
	}
	result, out = managementCall(t, c, "get_bug_report", bugReportGetInput{ID: r.ID})
	raw, _ = json.Marshal(result.StructuredContent)
	if result.IsError || out.Report == nil || out.Report.Summary != "untrusted body" || out.Report.Client == nil || strings.Contains(string(raw), "admin-id") || strings.Contains(string(raw), "private-owner-id") || strings.Contains(string(raw), "ownerId") {
		t.Fatal("detail or actor leak", string(raw))
	}
	for _, tc := range []struct {
		args any
		code string
	}{
		{bugReportStatusInput{ID: r.ID, State: "ignored", ExpectedVersion: r.ExpectedVersion}, "conflict"},
		{bugReportStatusInput{ID: r.ID, State: "closed", ExpectedVersion: r.ExpectedVersion}, "invalid-argument"},
		{bugReportStatusInput{ID: r.ID, State: "ignored", ExpectedVersion: r.ExpectedVersion, Note: strings.Repeat("가", 501)}, "invalid-argument"},
		{bugReportStatusInput{ID: "bug-missing", State: "ignored", ExpectedVersion: r.ExpectedVersion}, "not-found"},
	} {
		res, out := managementCall(t, c, "set_bug_report_status", tc.args)
		if !res.IsError || out.Code != tc.code {
			t.Fatal(fmt.Sprint(out), tc.code)
		}
	}
}

func TestBugReportManagementSchemaErrorsAreSafe(t *testing.T) {
	c, _, _ := managementFixture(t, "valid")
	for _, args := range []any{map[string]any{}, map[string]any{"id": "bug-missing", "actor": "forged"}} {
		result, out := managementCall(t, c, "get_bug_report", args)
		raw, _ := json.Marshal(result.StructuredContent)
		var fields map[string]any
		json.Unmarshal(raw, &fields)
		if !result.IsError || out.Code != "invalid-argument" || len(fields) != 2 {
			t.Fatal("schema error envelope", string(raw))
		}
	}
}
