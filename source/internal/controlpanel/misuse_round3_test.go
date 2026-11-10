package controlpanel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestMisuseH1RapidProjectToggleRetainsQueueAndCancelsStream(t *testing.T) {
	s := directStore(t)
	token, err := s.IssueToken("demo", "round3")
	if err != nil {
		t.Fatal("token fixture")
	}
	remote := broker.NewRemoteService()
	defer remote.Close()
	for i, name := range []string{"sender", "recipient"} {
		_, err := remote.AttachSession(t.Context(), broker.AttachRequest{Harness: "codex", SessionID: []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"}[i], PID: i + 1, CWD: "/synthetic", ChannelID: name + "-lease", Name: name})
		if err != nil {
			t.Fatal("attach fixture")
		}
	}
	if sent, err := remote.SendMessage(t.Context(), broker.Message{From: "sender", To: "recipient", Body: "retained"}); err != nil || !sent.Accepted {
		t.Fatal("queued fixture")
	}
	entered, done := make(chan struct{}), make(chan struct{})
	h := New(s, tokenViewBackend{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") == "true" {
			close(entered)
			<-r.Context().Done()
			return
		}
		w.WriteHeader(204)
	}))
	request := func(stream bool) *httptest.ResponseRecorder {
		path := "https://broker/mcp?project=demo"
		if stream {
			path += "&stream=true"
		}
		req := httptest.NewRequest("POST", path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(protocol.ProjectGenerationHeader, s.ProjectGeneration("demo"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	go func() { defer close(done); request(true) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("stream not entered")
	}
	for i := 0; i < 10; i++ {
		if err := s.SaveProject("demo", "token", false, ""); err != nil {
			t.Fatal("disable")
		}
		denied := request(false)
		if denied.Code != 403 || !strings.Contains(denied.Body.String(), "project_disabled") || denied.Header().Get("Retry-After") != "30" {
			t.Fatal("disable response lost retry policy")
		}
		if err := s.SaveProject("demo", "token", true, ""); err != nil {
			t.Fatal("enable")
		}
		if request(false).Code != 204 {
			t.Fatal("enabled project retained refusal")
		}
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled stream not cancelled")
	}
	waitCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	msg, err := remote.WaitForIngress(waitCtx, "recipient", "recipient-lease")
	if err != nil || msg.Body != "retained" {
		t.Fatal("queued message lost during policy churn")
	}
}

func TestMisuseH2DeleteDrainsAndFencesAllMCPWork(t *testing.T) {
	s := directStore(t)
	token, err := s.IssueToken("demo", "round3")
	if err != nil {
		t.Fatal("fixture token")
	}
	grant, changed, ok := s.authorize("demo", token)
	if !ok {
		t.Fatal("authorize fixture")
	}
	old := s.ProjectGeneration("demo")
	// Handover, rebind, send and register all enter this common MCP operation gate.
	var releases []func()
	for i := 0; i < 4; i++ {
		release, err := s.startProjectOperation("demo", changed)
		if err != nil {
			t.Fatal("operation fixture")
		}
		defer release()
		releases = append(releases, release)
	}
	idle, err := s.beginProjectDeletion("admin", "demo", "demo")
	if err != nil {
		t.Fatal("begin deletion")
	}
	if s.CreateProject("demo") == nil {
		t.Fatal("recreated during deletion")
	}
	if _, err := s.startProjectOperation("demo", changed); err == nil {
		t.Fatal("admitted old generation work")
	}
	if s.finishProjectDeletion("demo", old) == nil {
		t.Fatal("deleted with active operation")
	}
	h := New(s, tokenViewBackend{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("old request reached backend") }))
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "https://broker/mcp?project=demo", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(protocol.ProjectGenerationHeader, old)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	pending := request()
	if pending.Code != 403 || !strings.Contains(pending.Body.String(), "project_deleting") || pending.Header().Get("Retry-After") != "30" {
		t.Fatal("deleting response missing")
	}
	for _, release := range releases {
		select {
		case <-idle:
			t.Fatal("deletion did not wait for all old work")
		default:
		}
		release()
	}
	<-idle
	if err := s.finishProjectDeletion("demo", old); err != nil {
		t.Fatal("finish deletion")
	}
	if err := s.CreateProject("demo"); err != nil {
		t.Fatal("recreate")
	}
	if s.ProjectGeneration("demo") == old {
		t.Fatal("generation reused")
	}
	if grant.Check != nil && grant.Check("synthetic") == nil {
		t.Fatal("old grant usable after recreation")
	}
	if _, err := s.startProjectOperation("demo", changed); err == nil {
		t.Fatal("old operation entered recreated project")
	}
	deleted := request()
	if deleted.Code != 410 || !strings.Contains(deleted.Body.String(), "project_deleted") {
		t.Fatal("old generation not terminal")
	}
}

func TestMisuseH3GenerationHintsNeverAuthorizeOrDisclose(t *testing.T) {
	s := directStore(t)
	token, err := s.IssueToken("demo", "round3")
	if err != nil {
		t.Fatal("fixture token")
	}
	if err := s.CreateProject("other"); err != nil {
		t.Fatal("other project")
	}
	generation := s.ProjectGeneration("demo")
	h := New(s, tokenViewBackend{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for _, hints := range [][]string{{""}, {strings.Repeat("X", 65536)}, {s.ProjectGeneration("other")}, {generation, strings.Repeat("Z", 26)}, {strings.Repeat("Z", 26), generation}} {
		for _, valid := range []bool{false, true} {
			req := httptest.NewRequest("POST", "https://broker/mcp?project=demo", nil)
			for _, hint := range hints {
				req.Header.Add(protocol.ProjectGenerationHeader, hint)
			}
			if valid {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if valid && w.Code != 204 || !valid && (w.Code < 400 || w.Code >= 500) {
				t.Fatalf("hint changed authorization: %d", w.Code)
			}
			if !valid && (strings.Contains(w.Body.String(), generation) || w.Header().Get(protocol.ProjectGenerationHeader) != "") {
				t.Fatal("unauthenticated response disclosed generation")
			}
		}
	}
}
