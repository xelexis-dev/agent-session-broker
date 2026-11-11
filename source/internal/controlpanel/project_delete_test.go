package controlpanel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type deletingBackend struct {
	panelBackend
	deleted []string
	fail    bool
}

func TestProjectDeleteDrainsOnlyTargetAndBlocksNewWork(t *testing.T) {
	s := userStore(t)
	if err := s.SaveProjectFor("owner", "demo", "token", true); err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueTokenFor("owner", "demo", "deleting-agent", "owner")
	if err != nil {
		t.Fatal(err)
	}
	entered, cancelled, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	b := &deletingBackend{}
	h := New(s, b, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(cancelled)
		<-finish
	}))
	c := loginPanelUser(t, h, "owner")
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		r := httptest.NewRequest("POST", "https://broker/mcp?project=demo", nil)
		r.Header.Set("Authorization", "Bearer "+raw)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}()
	<-entered
	deleteDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		deleteDone <- c.call("POST", "/projects/delete", url.Values{"project": {"demo"}, "confirmName": {"demo"}})
	}()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("live request not cancelled")
	}
	select {
	case <-deleteDone:
		t.Fatal("deleted before backend request ended")
	default:
	}
	if _, err := s.startProjectOperation("demo", nil); err == nil {
		t.Fatal("new backend operation during delete")
	}
	if err := s.CreateProjectFor("owner", "unrelated"); err != nil {
		t.Fatal(err)
	}
	release, err := s.startProjectOperation("unrelated", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	close(finish)
	<-requestDone
	select {
	case w := <-deleteDone:
		if w.Code != 303 {
			t.Fatal(w.Code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("delete waited for unrelated operation")
	}
}

func TestProjectDeletePersistsPendingAndRollsBackStorageFailure(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Setup(testPassword); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateProject("demo"); err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueToken("demo", "deleting-agent")
	if err != nil {
		t.Fatal(err)
	}
	_, generation, allowed := s.authorize("demo", raw)
	if !allowed {
		t.Fatal("fixture authorization")
	}
	write := s.write
	s.write = func([]byte) error { return errors.New("fixture storage failed") }
	if _, err = s.beginProjectDeletion("admin", "demo", "demo"); err == nil {
		t.Fatal("failed persistence accepted")
	}
	select {
	case <-generation:
		t.Fatal("failure revoked connection")
	default:
	}
	if s.data.Projects["demo"].Deleting {
		t.Fatal("failure mutated state")
	}
	s.write = write
	if _, err = s.beginProjectDeletion("admin", "demo", "demo"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.ImportProjects([]string{"demo"}); err != nil {
		t.Fatal(err)
	}
	if !s.data.Projects["demo"].Deleting || s.Authorize("demo", "") {
		t.Fatal("restart/import reopened pending deletion")
	}
	if _, err = s.beginProjectDeletion("admin", "demo", "demo"); err != nil {
		t.Fatal(err)
	}
	if err = s.finishProjectDeletion("demo"); err != nil {
		t.Fatal(err)
	}
	if len(s.Projects()) != 0 {
		t.Fatal("retry did not finish")
	}
}

func (b *deletingBackend) DeleteProject(_ context.Context, name string) error {
	if b.fail {
		return errors.New("fixture delete failed")
	}
	b.deleted = append(b.deleted, name)
	return nil
}

func TestProjectDeleteOwnerConfirmationAndRetry(t *testing.T) {
	s := userStore(t)
	b := &deletingBackend{}
	h := New(s, b, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, actor := range []string{"developer", "viewer", "outsider"} {
		c := loginPanelUser(t, h, actor)
		if w := c.call("POST", "/projects/delete", url.Values{"project": {"demo"}, "confirmName": {"demo"}}); w.Code != 403 {
			t.Fatalf("%s delete status=%d", actor, w.Code)
		}
	}
	c := loginPanelUser(t, h, "owner")
	for _, name := range []string{"", "DEMO", " demo"} {
		if w := c.call("POST", "/projects/delete", url.Values{"project": {"demo"}, "confirmName": {name}}); w.Code != 400 {
			t.Fatalf("confirmation %q status=%d", name, w.Code)
		}
	}
	req := httptest.NewRequest("POST", "http://broker/projects/delete", strings.NewReader("project=demo&confirmName=demo"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c.cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 || len(b.deleted) != 0 {
		t.Fatal("missing CSRF deleted project")
	}
	b.fail = true
	if w = c.call("POST", "/projects/delete", url.Values{"project": {"demo"}, "confirmName": {"demo"}}); w.Code != 503 {
		t.Fatalf("backend failure status=%d", w.Code)
	}
	if s.Authorize("demo", "") || s.CreateProjectFor("owner", "demo") == nil || s.SaveProjectFor("owner", "demo", "token", true) == nil {
		t.Fatal("pending deletion reopened")
	}
	if w = c.call("GET", "/projects/settings?project=demo", nil); !strings.Contains(w.Body.String(), en("tokens.deletion_is_pending_and_access_is_blocked")) || !strings.Contains(w.Body.String(), en("projects.retry_permanent_deletion")) {
		t.Fatal("missing retry UI")
	}
	b.fail = false
	if w = c.call("POST", "/projects/delete", url.Values{"project": {"demo"}, "confirmName": {"demo"}}); w.Code != 303 {
		t.Fatalf("retry status=%d", w.Code)
	}
	if len(b.deleted) != 1 || b.deleted[0] != "demo" || len(s.ProjectsFor("owner")) != 0 {
		t.Fatal("project not deleted")
	}
	if err := s.CreateProjectFor("owner", "demo"); err != nil {
		t.Fatal(err)
	}
	if len(s.ProjectsFor("owner")[0].Tokens) != 0 {
		t.Fatal("tokens survived recreation")
	}
}
