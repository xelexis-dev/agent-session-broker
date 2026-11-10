package controlpanel

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type round3HistoryBackend struct {
	*reportPanelBackend
	store *Store
}

func (b round3HistoryBackend) DeleteProject(_ context.Context, project string) error {
	return b.reports.DeleteProject(project, b.store.ProjectGeneration(project))
}
func TestRound3J3MixedHistoryAcrossDeleteRecreateAndPageBoundary(t *testing.T) {
	// 51 historical rows: the first page is mixed, the second unknown-only.
	// Current rows interspersed in storage must never consume history page slots.
	kinds := []string{"current", "previous"}
	for range 50 {
		kinds = append(kinds, "unknown")
	}
	s, reports, _ := generationHistoryFixture(t, kinds)
	h := New(s, round3HistoryBackend{&reportPanelBackend{reports: reports}, s}, http.NotFoundHandler())
	reader := loginPanelUser(t, h, "admin")
	deleter := loginPanelUser(t, h, "admin")
	owner := loginPanelUser(t, h, "owner")
	developer := loginPanelUser(t, h, "developer")
	checkHistory := func() {
		t.Helper()
		for _, tc := range []struct {
			page, notice string
			rows         int
		}{{"1", historyMixedNotice, 50}, {"2", historyUnknownNotice, 1}, {"99", historyUnknownNotice, 1}} {
			w := reader.call("GET", "/admin/bug-reports?bucket=history&page="+tc.page, nil)
			body := w.Body.String()
			if w.Code != 200 || !strings.Contains(body, "총 51건") || !strings.Contains(body, tc.notice) {
				t.Fatalf("history page %s: count/notice status=%d", tc.page, w.Code)
			}
			if got := strings.Count(body, `href="/admin/bug-reports/detail?`); got != tc.rows {
				t.Fatalf("history page %s rows=%d want=%d", tc.page, got, tc.rows)
			}
			if strings.Contains(body, "bug-current-000") {
				t.Fatal("current row consumed history page")
			}
		}
	}
	checkHistory()
	for _, path := range []string{"/projects/audit?project=demo&bucket=history", "/admin/bug-reports?bucket=history", "/admin/bug-reports/detail?id=bug-previous-001&bucket=history", "/admin/bug-reports/detail?id=bug-unknown-051&bucket=unknown"} {
		if owner.call("GET", path, nil).Code != 403 || developer.call("GET", path, nil).Code != 403 {
			t.Fatal("non-admin guessed history URL")
		}
	}
	oldGeneration := s.ProjectGeneration("demo")
	codes := concurrentRound2(t, func() int { return reader.call("GET", "/admin/bug-reports?bucket=history&page=2", nil).Code }, func() int {
		return deleter.call("POST", "/projects/delete", url.Values{"project": {"demo"}, "confirmName": {"demo"}}).Code
	})
	if len(codes) != 2 || !((codes[0] == 200 && codes[1] == 303) || (codes[0] == 303 && codes[1] == 200)) {
		t.Fatalf("read/delete statuses=%v", codes)
	}
	checkHistory()
	if err := s.CreateProjectFor("outsider", "demo"); err != nil {
		t.Fatal(err)
	}
	if s.ProjectGeneration("demo") == oldGeneration {
		t.Fatal("recreated project reused generation")
	}
	checkHistory()
	for _, path := range []string{"/projects/audit?project=demo", "/admin/bug-reports?page=99"} {
		w := reader.call("GET", path, nil)
		if w.Code != 200 {
			t.Fatalf("current page=%d", w.Code)
		}
		for _, old := range []string{"worker-current", "worker-previous", "worker-unknown", "보고 내용 current", "보고 내용 previous", "보고 내용 unknown"} {
			if strings.Contains(w.Body.String(), old) {
				t.Fatal("deleted/recreated project inherited report/audit")
			}
		}
	}
	for i, kind := range kinds {
		id := fmt.Sprintf("bug-%s-%03d", kind, i)
		if reader.call("GET", "/admin/bug-reports/detail?id="+id, nil).Code != 404 {
			t.Fatal("old record accessible as current")
		}
		w := reader.call("POST", "/admin/bug-reports/status", url.Values{"id": {id}, "status": {"resolved"}, "expectedVersion": {"historical-fixture-version"}})
		if w.Code != 404 {
			t.Fatalf("old status change=%d", w.Code)
		}
	}
	for _, path := range []string{"/projects/audit?project=demo&bucket=history", "/admin/bug-reports/detail?id=bug-previous-001&bucket=history"} {
		if owner.call("GET", path, nil).Code != 403 || developer.call("GET", path, nil).Code != 403 {
			t.Fatal("old project member inherited history after recreation")
		}
	}
}
