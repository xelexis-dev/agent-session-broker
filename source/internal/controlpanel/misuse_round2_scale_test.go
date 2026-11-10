package controlpanel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

func TestRound2F6ThousandSessionRows(t *testing.T) {
	_, b, h := round2Fixture(t)
	b.rows["demo"] = make([]Session, 1000)
	for i := range b.rows["demo"] {
		b.rows["demo"][i] = Session{ID: fmt.Sprintf("scale-%04d", i), Name: fmt.Sprintf("worker-%04d", i), OwnerID: "owner", Harness: "codex", SessionID: fmt.Sprintf("conversation-%04d", i), ClientID: "synthetic-pc", Status: "alive", Ready: true}
	}
	c := loginPanelUser(t, h, "owner")
	start := time.Now()
	w := c.call("GET", "/projects/sessions?project=demo", nil)
	elapsed := time.Since(start)
	if w.Code != 200 || strings.Count(w.Body.String(), `data-session-row="`) != 1000 || !strings.Contains(w.Body.String(), "</html>") {
		t.Fatal("session HTML lost rows or failed")
	}
	t.Logf("session HTML: rows=1000 elapsed=%s bytes=%d (no server paging)", elapsed, w.Body.Len())
	start = time.Now()
	live := c.call("GET", "/projects/sessions/live?project=demo", nil)
	elapsed = time.Since(start)
	var payload struct {
		Rows []liveSessionRow `json:"rows"`
	}
	if live.Code != 200 || json.Unmarshal(live.Body.Bytes(), &payload) != nil || len(payload.Rows) != 1000 {
		t.Fatal("large live response failed")
	}
	keys := map[string]bool{}
	for _, row := range payload.Rows {
		keys[row.Key] = true
	}
	if len(keys) != 1000 || !keys[b.rows["demo"][999].RowKey()] {
		t.Fatal("live response lost or duplicated rows")
	}
	t.Logf("session live: elapsed=%s bytes=%d", elapsed, live.Body.Len())
}
func TestRound2F6TwoHundredProjectCreationAttempts(t *testing.T) {
	s := userStore(t)
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	c := loginPanelUser(t, h, "admin")
	accepted, rejected := 1, 0 // fixture starts with one project.
	for i := 1; i < 200; i++ {
		w := c.call("POST", "/projects/create", url.Values{"name": {fmt.Sprintf("scale-%03d", i)}})
		if w.Code == 303 {
			accepted++
		} else if w.Code == 400 {
			rejected++
		} else {
			t.Fatalf("create=%d", w.Code)
		}
	}
	if accepted != 128 || rejected != 72 || len(s.ProjectsFor("admin")) != 128 {
		t.Fatal("project cap not enforced")
	}
	start := time.Now()
	w := c.call("GET", "/", nil)
	elapsed := time.Since(start)
	if w.Code != 200 || strings.Count(w.Body.String(), `data-filter-text="`) != 128 || !strings.Contains(w.Body.String(), "</html>") {
		t.Fatal("maximum-size project table failed")
	}
	t.Logf("projects: attempted=200 accepted=128 rejected=72 elapsed=%s bytes=%d (no server paging)", elapsed, w.Body.Len())
}
func round2ReportScaleStore(t *testing.T) *bugreport.Store {
	t.Helper()
	dir := t.TempDir()
	// Bulk seed valid persisted records, then reopen through the real report store.
	// This measures read/paging behavior, not submission quotas or Create throughput.
	db, err := sqlitestore.Open(filepath.Join(dir, "bugreports"), "bugreports.db")
	if err != nil {
		t.Fatal(err)
	}
	records := map[string][]byte{}
	now := time.Now().UTC()
	for i := 0; i < 2000; i++ {
		severity := "low"
		if i%2 == 0 {
			severity = "high"
		}
		report := bugreport.Report{Input: bugreport.Input{Title: fmt.Sprintf("scale report %04d", i), Summary: "synthetic scale fixture", Severity: severity}, ID: fmt.Sprintf("bug-scale-%04d", i), Reporter: bugreport.Reporter{ProjectGenerationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", Project: "demo", SessionName: "scale-agent", RegistrationID: fmt.Sprintf("scale-reg-%04d", i), OwnerID: "owner", Harness: "codex"}, ReceivedAt: now.Add(-time.Duration(i) * time.Second), ServerVersion: "test", Status: "open"}
		raw, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		records[report.ID] = raw
	}
	if err = db.Batch(records, nil); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := bugreport.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
func TestRound2F6TwoThousandReportsPaging(t *testing.T) {
	reports := round2ReportScaleStore(t)
	h := New(userStore(t), &reportPanelBackend{reports: reports}, http.NotFoundHandler())
	c := loginPanelUser(t, h, "admin")
	seen := map[string]bool{}
	for page := 1; page <= 40; page++ {
		rows, total, err := reports.List(bugreport.Filter{}, page)
		if err != nil || total != 2000 || len(rows) != 50 {
			t.Fatal("report page count incorrect")
		}
		for _, r := range rows {
			if seen[r.ID] {
				t.Fatal("report duplicated across pages")
			}
			seen[r.ID] = true
		}
	}
	if len(seen) != 2000 {
		t.Fatal("report paging lost rows")
	}
	link := regexp.MustCompile(`href="/admin/bug-reports/detail\?[^\"]+"`)
	for _, tc := range []struct{ query, indicator string }{{"?status=all&page=1", "1 / 40"}, {"?status=all&page=40", "40 / 40"}, {"?status=all&page=9999", "40 / 40"}, {"?status=all&severity=high&page=20", "20 / 20"}} {
		start := time.Now()
		w := c.call("GET", "/admin/bug-reports"+tc.query, nil)
		elapsed := time.Since(start)
		if w.Code != 200 || len(link.FindAllString(w.Body.String(), -1)) != 50 || !strings.Contains(w.Body.String(), tc.indicator) {
			t.Fatalf("report list incorrect for %s", tc.query)
		}
		t.Logf("reports %s: rows=50 elapsed=%s bytes=%d", tc.query, elapsed, w.Body.Len())
	}
}
