package bugreport

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func fixtureStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, &now
}
func reporter(n int) Reporter {
	return Reporter{ProjectGenerationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", Project: "project", SessionName: fmt.Sprint("worker-", n), RegistrationID: fmt.Sprint("reg-", n), Harness: "codex", OwnerID: "owner"}
}
func fixtureInput() Input { return Input{Title: "defect", Summary: "observed symptom"} }
func TestStoreRedactsPersistsFiltersAndRecordsActor(t *testing.T) {
	s, now := fixtureStore(t)
	token := strings.Join([]string{"asb", "p", "syntheticsecret"}, "_")
	in := fixtureInput()
	in.Title = token
	in.Summary = "ordinary-random-string " + token
	in.Client = &ClientDiagnostics{ClientVersion: token}
	r, err := s.Create(in, reporter(1), "0.17.1")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), token) || r.Title != "<redacted>" || !strings.Contains(r.Summary, "ordinary-random-string") {
		t.Fatal("redaction violated")
	}
	*now = now.Add(time.Minute)
	if err = s.SetStatus(r.ID, "closed", "admin"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(r.ID)
	if err != nil || got.Status != "closed" || got.StatusChangedBy != "admin" || !got.StatusChangedAt.Equal(*now) {
		t.Fatal("status audit missing")
	}
	rows, total, err := s.List(Filter{Status: "closed", Severity: "medium"}, 1)
	if err != nil || total != 1 || len(rows) != 1 || rows[0].ID != r.ID {
		t.Fatal("filter failed")
	}
	_, total, _ = s.List(Filter{Status: "open"}, 1)
	if total != 0 {
		t.Fatal("status filter ignored")
	}
}
func TestValidationLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Input)
	}{
		{"blank-title", func(i *Input) { i.Title = " " }},
		{"blank-summary", func(i *Input) { i.Summary = "" }},
		{"title", func(i *Input) { i.Title = strings.Repeat("가", 201) }},
		{"summary", func(i *Input) { i.Summary = strings.Repeat("x", 4001) }},
		{"steps", func(i *Input) { i.Steps = strings.Repeat("x", 4001) }},
		{"expected", func(i *Input) { i.Expected = strings.Repeat("x", 2001) }},
		{"actual", func(i *Input) { i.Actual = strings.Repeat("x", 2001) }},
		{"tool", func(i *Input) { i.Tool = strings.Repeat("x", 65) }},
		{"severity", func(i *Input) { i.Severity = "urgent" }},
		{"bytes", func(i *Input) { i.Client = &ClientDiagnostics{ClientVersion: strings.Repeat("x", MaxBytes)} }},
		{"events", func(i *Input) { i.Client = &ClientDiagnostics{Events: make([]DiagnosticEvent, 21)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := fixtureInput()
			tc.change(&in)
			if !errors.Is(Validate(in), ErrInvalid) {
				t.Fatal("invalid input accepted")
			}
		})
	}
	in := Input{Title: strings.Repeat("가", 200), Summary: strings.Repeat("x", 4000), Steps: strings.Repeat("x", 4000), Expected: strings.Repeat("x", 2000), Actual: strings.Repeat("x", 2000), Tool: strings.Repeat("x", 64), Severity: "critical"}
	if err := Validate(in); err != nil {
		t.Fatal("valid boundary rejected")
	}
}
func TestQuotaRetryAndPersistence(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		if _, err = s.Create(fixtureInput(), reporter(1), "test"); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return now }
	_, err = s.Create(fixtureInput(), reporter(1), "test")
	var limited *RateLimitError
	if !errors.As(err, &limited) || !limited.RetryAt.Equal(now.Add(time.Hour)) {
		t.Fatal("hourly quota/retry not persisted")
	}
	now = now.Add(time.Hour)
	for i := 0; i < 190; i++ {
		if _, err = s.Create(fixtureInput(), reporter(2+i/10), "test"); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.Create(fixtureInput(), reporter(90), "test")
	if !errors.As(err, &limited) || !limited.RetryAt.Equal(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("daily quota or retry incorrect")
	}
	now = now.Add(24 * time.Hour)
	if _, err = s.Create(fixtureInput(), reporter(1), "test"); err != nil {
		t.Fatal("expired quota still blocks")
	}
}
func TestRetentionPaginationAndProjectDeletion(t *testing.T) {
	s, now := fixtureStore(t)
	puts := map[string][]byte{}
	for i := 0; i < 2002; i++ {
		r := Report{Input: fixtureInput(), ID: fmt.Sprintf("bug-%04d", i), Reporter: reporter(i), Status: "open", ReceivedAt: now.Add(-time.Duration(i+1) * time.Minute)}
		r.Reporter.Project = fmt.Sprint("p-", i)
		raw, _ := json.Marshal(r)
		puts[r.ID] = raw
	}
	ancient := Report{Input: fixtureInput(), ID: "bug-ancient", Reporter: reporter(1), Status: "open", ReceivedAt: now.Add(-Retention)}
	raw, _ := json.Marshal(ancient)
	puts[ancient.ID] = raw
	if err := s.db.Batch(puts, nil); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Create(fixtureInput(), reporter(1), "test")
	if err != nil {
		t.Fatal(err)
	}
	rows, total, err := s.List(Filter{}, 1)
	if err != nil || total != 2000 || len(rows) != PageSize || rows[0].ID != fresh.ID {
		t.Fatal("retention/order/page incorrect")
	}
	rows, total, err = s.List(Filter{}, 40)
	if err != nil || total != 2000 || len(rows) != 50 {
		t.Fatal("last page incorrect")
	}
	rows, _, err = s.List(Filter{}, 41)
	if err != nil || len(rows) != 0 {
		t.Fatal("out-of-range page incorrect")
	}
	if _, err = s.Get(ancient.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired report survived")
	}
	if _, err = s.Get("bug-2001"); !errors.Is(err, ErrNotFound) {
		t.Fatal("oldest report survived count limit")
	}
	if err = s.DeleteProject("project", "AAAAAAAAAAAAAAAAAAAAAAAAAA"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(fresh.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("project report survived delete")
	}
	_, total, err = s.List(Filter{}, 1)
	if err != nil || total != 1999 {
		t.Fatal("neighbor reports changed")
	}
	*now = now.Add(Retention)
	_, total, err = s.List(Filter{}, 1)
	if err != nil || total != 0 {
		t.Fatal("age retention ignored on reads")
	}
}
func TestStoreInvalidStateAndFailedWrite(t *testing.T) {
	s, _ := fixtureStore(t)
	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing report error")
	}
	if err := s.SetStatus("missing", "invalid", "admin"); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid status accepted")
	}
	if _, _, err := s.List(Filter{Severity: "invalid"}, 1); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid filter accepted")
	}
	_ = s.Close()
	if _, err := s.Create(fixtureInput(), reporter(1), "test"); err == nil {
		t.Fatal("failed database write reported success")
	}
}

func TestQuotaSurvivesReportRetentionAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		if _, err = s.Create(fixtureInput(), reporter(1), "test"); err != nil {
			t.Fatal(err)
		}
	}
	// Fill the report retention window with newer reports from other projects.
	// Seed existing persisted report shapes to keep the test independent of SQL speed.
	puts := map[string][]byte{}
	for i := 0; i < MaxReports; i++ {
		r := Report{Input: fixtureInput(), ID: fmt.Sprintf("bug-new-%04d", i), Reporter: reporter(2 + i/10), Status: "open", ReceivedAt: now.Add(time.Minute)}
		r.Reporter.Project = fmt.Sprint("other-", i/200)
		raw, _ := json.Marshal(r)
		puts[r.ID] = raw
	}
	if err = s.db.Batch(puts, nil); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	trigger := reporter(99)
	trigger.Project = "trigger"
	if _, err = s.Create(fixtureInput(), trigger, "test"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return now }
	_, err = s.Create(fixtureInput(), reporter(1), "test")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatal("report retention erased the registration rate limit after restart")
	}
	if err = s.DeleteProject("project", "AAAAAAAAAAAAAAAAAAAAAAAAAA"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Create(fixtureInput(), reporter(1), "test"); err != nil {
		t.Fatal("deleted project retained its rate limit")
	}
}

func TestUserTokenRedaction(t *testing.T) {
	s, _ := fixtureStore(t)
	in := fixtureInput()
	secret := strings.Join([]string{"asb", "u", "syntheticsecret"}, "_")
	in.Summary = secret
	report, err := s.Create(in, reporter(1), "dev")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), secret) {
		t.Fatal("user credential leaked")
	}
}
