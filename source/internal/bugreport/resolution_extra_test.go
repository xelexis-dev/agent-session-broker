package bugreport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func resolutionFixture(t *testing.T) (*Store, Report, Actor) {
	t.Helper()
	s, _ := fixtureStore(t)
	r, err := s.Create(fixtureInput(), reporter(1), "test")
	if err != nil {
		t.Fatal(err)
	}
	s.SetProjectPolicy(func(actor string) (map[string]string, func(), error) {
		if actor != "admin" && actor != "" {
			return nil, nil, ErrForbidden
		}
		return map[string]string{"project": reporter(1).ProjectGenerationID}, func() {}, nil
	})
	return s, r, Actor{UserID: "admin", Source: "panel"}
}
func TestResolutionTransitionsNotesNoOpConflict(t *testing.T) {
	s, r, a := resolutionFixture(t)
	token := strings.Join([]string{"asb", "s", "syntheticnote"}, "_")
	note := "fixed\n" + token
	for _, state := range []string{"resolved", "ignored", "open", "resolved"} {
		before := r
		var changed bool
		var err error
		r, changed, err = s.SetResolution(r.ID, state, note, a, r.ExpectedVersion)
		if err != nil || !changed || r.State() != state || r.Note != "fixed\n<redacted>" || len(r.StatusChanges) != len(before.StatusChanges)+1 {
			t.Fatalf("transition %s: %v", state, err)
		}
		raw, _ := s.db.Get(r.ID)
		sum := sha256.Sum256(raw)
		if r.ExpectedVersion != hex.EncodeToString(sum[:]) || strings.Contains(string(raw), token) {
			t.Fatal("version/redaction")
		}
		noop, changed, err := s.SetResolution(r.ID, state, note, a, r.ExpectedVersion)
		after, _ := s.db.Get(r.ID)
		if err != nil || changed || noop.ExpectedVersion != r.ExpectedVersion || string(raw) != string(after) {
			t.Fatal("no-op wrote")
		}
		if _, _, err = s.SetResolution(r.ID, state, note, a, before.ExpectedVersion); !errors.Is(err, ErrConflict) {
			t.Fatal("stale same-state must conflict")
		}
	}
	r, changed, err := s.SetResolution(r.ID, r.State(), "note edited", a, r.ExpectedVersion)
	if err != nil || !changed || r.StatusChanges[len(r.StatusChanges)-1].From != r.State() {
		t.Fatal("note-only change must be recorded")
	}
	for _, note := range []string{strings.Repeat("가", 501), "\xff", "tab\t", "return\r", "nul\x00", "del\x7f", "next\u0085"} {
		if _, _, err = s.SetResolution(r.ID, "open", note, a, r.ExpectedVersion); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid note accepted %q", note)
		}
	}
	if _, _, err = s.SetResolution(r.ID, "open", strings.Repeat("가", 500), a, r.ExpectedVersion); err != nil {
		t.Fatal("500 runes rejected", err)
	}
}
func TestResolutionHistoryLimitsAndLegacyToggle(t *testing.T) {
	s, r, a := resolutionFixture(t)
	var err error
	for i := 0; i < 25; i++ {
		r, _, err = s.SetResolution(r.ID, "ignored", fmt.Sprint(i), a, r.ExpectedVersion)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(r.StatusChanges) != 20 || !r.HistoryTruncated || r.StatusChanges[0].Note != "5" {
		t.Fatal("20 event cap")
	}
	for i := 0; i < 20; i++ {
		r, _, err = s.SetResolution(r.ID, "resolved", fmt.Sprint(i)+strings.Repeat("가", 490), a, r.ExpectedVersion)
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(r.StatusChanges)
	if len(raw) > 16*1024 || len(r.StatusChanges) >= 20 || len(r.StatusChanges) == 0 || !strings.HasPrefix(r.StatusChanges[len(r.StatusChanges)-1].Note, "19") {
		t.Fatal("history bytes/newest cap")
	}
	previous := r
	r, changed, err := s.SetStatusVersion(r.ID, "closed", "admin", r.ExpectedVersion)
	if err != nil || changed || r.Resolution != "resolved" || len(r.StatusChanges) != len(previous.StatusChanges) {
		t.Fatal("legacy close erased classification/history")
	}
	r, _, err = s.SetStatusVersion(r.ID, "open", "admin", r.ExpectedVersion)
	if err != nil || r.Resolution != "" || r.Status != "open" || len(r.StatusChanges) < 2 || r.StatusChanges[len(r.StatusChanges)-2].To != "resolved" {
		t.Fatal("legacy reopen erased history")
	}
	if _, _, err = s.SetStatusVersion(r.ID, "closed", "admin", previous.ExpectedVersion); !errors.Is(err, ErrConflict) {
		t.Fatal("legacy stale write")
	}
}
func TestResolutionConcurrentCASAndReopenStore(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(fixtureInput(), reporter(1), "test")
	if err != nil {
		t.Fatal(err)
	}
	s.SetProjectPolicy(func(string) (map[string]string, func(), error) {
		return map[string]string{"project": reporter(1).ProjectGenerationID}, func() {}, nil
	})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, state := range []string{"resolved", "ignored"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, e := s.SetResolution(r.ID, state, "", Actor{UserID: "admin", Source: "panel"}, r.ExpectedVersion)
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for e := range results {
		if e == nil {
			success++
		} else if errors.Is(e, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal("CAS was not atomic")
	}
	before, _ := s.Get(r.ID)
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Get(r.ID)
	if err != nil || got.ExpectedVersion != before.ExpectedVersion || len(got.StatusChanges) != 1 {
		t.Fatal("restart lost version/history")
	}
}
func TestResolutionRecordLimitAndInputUnchanged(t *testing.T) {
	s, r, a := resolutionFixture(t)
	// Seed a large persisted record to exercise the independent record envelope.
	r.Client = &ClientDiagnostics{LastFollow: strings.Repeat("x", MaxRecordBytes-3000)}
	raw, _ := json.Marshal(r)
	if len(raw) >= MaxRecordBytes {
		t.Fatal("fixture")
	}
	if err := s.db.Put(r.ID, raw); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err = s.SetResolution(r.ID, "resolved", "boundary", a, r.ExpectedVersion)
	if err != nil {
		t.Fatal("large record should remain mutable", err)
	}
	raw, _ = s.db.Get(r.ID)
	if len(raw) <= MaxBytes || len(raw) > MaxRecordBytes {
		t.Fatal("record limit")
	}
	if Validate(r.Input) != ErrInvalid {
		t.Fatal("submission limit expanded")
	}
	r.Client.LastFollow += strings.Repeat("x", 3000)
	raw, _ = json.Marshal(r)
	if err = s.db.Put(r.ID, raw); err != nil {
		t.Fatal(err)
	}
	r, err = s.Get(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.SetResolution(r.ID, "ignored", "boundary", a, r.ExpectedVersion); err != ErrInvalid {
		t.Fatal("record overflow accepted")
	}
	after, _ := s.db.Get(r.ID)
	if string(raw) != string(after) {
		t.Fatal("failed mutation changed bytes")
	}
}
func TestResolutionPolicyAndGenerationFence(t *testing.T) {
	s, r, a := resolutionFixture(t)
	a.Source = "mcp"
	a.Project = "project"
	a.ProjectGenerationID = reporter(1).ProjectGenerationID
	a.Session = &ChangeSession{Name: "worker", RegistrationID: "reg"}
	if _, err := s.GetProject("outsider", a.Project, a.ProjectGenerationID, r.ID); err != ErrForbidden {
		t.Fatal("nonadmin")
	}
	if _, _, err := s.ListProject("admin", a.Project, "old", "all", "", 1); err != ErrForbidden {
		t.Fatal("old grant")
	}
	// Same redacted display name is never used for scope matching.
	other := r
	other.ID = "bug-other"
	other.Reporter.ProjectGenerationID = "BBBBBBBBBBBBBBBBBBBBBBBBBB"
	other.Reporter.Project = "project"
	raw, _ := json.Marshal(other)
	s.db.Put(other.ID, raw)
	unknown := other
	unknown.ID = "bug-unknown"
	unknown.Reporter.ProjectGenerationID = ""
	raw, _ = json.Marshal(unknown)
	s.db.Put(unknown.ID, raw)
	for _, id := range []string{other.ID, unknown.ID, "bug-missing"} {
		if _, err := s.GetProject("admin", a.Project, a.ProjectGenerationID, id); err != ErrNotFound {
			t.Fatal("scope leak", id, err)
		}
		if _, _, err := s.SetResolution(id, "resolved", "", a, r.ExpectedVersion); err != ErrNotFound {
			t.Fatal("scope write", id, err)
		}
	}
	rows, total, err := s.ListProject("admin", a.Project, a.ProjectGenerationID, "all", "", 1)
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatal("scope before pagination")
	}
	// Legacy closed remains explicitly closed and can be classified later.
	if err = s.SetStatus(r.ID, "closed", "admin"); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Get(r.ID)
	if r.State() != "closed" {
		t.Fatal("legacy inferred resolved")
	}
	r, _, err = s.SetResolution(r.ID, "ignored", "", a, r.ExpectedVersion)
	if err != nil || r.State() != "ignored" {
		t.Fatal("legacy reclassification")
	}
}
func TestResolutionRollback0172Reader(t *testing.T) {
	s, r, a := resolutionFixture(t)
	r, _, err := s.SetResolution(r.ID, "ignored", "reason", a, r.ExpectedVersion)
	if err != nil {
		t.Fatal(err)
	}
	// The 0.17.2 JSON shape and rowsLocked key rules intentionally ignore extensions.
	type oldReport struct {
		Input
		ID         string    `json:"id"`
		Reporter   Reporter  `json:"reporter"`
		ReceivedAt time.Time `json:"receivedAt"`
		Status     string    `json:"status"`
	}
	rows, err := s.db.List()
	if err != nil {
		t.Fatal(err)
	}
	closed := 0
	for key, raw := range rows {
		if strings.HasPrefix(key, "quota-") {
			continue
		}
		var old oldReport
		if json.Unmarshal(raw, &old) != nil || old.ID != key || !strings.HasPrefix(key, "bug-") {
			t.Fatal("0.17.2 rowsLocked rejects record")
		}
		if old.Status == "closed" {
			closed++
		}
	}
	if closed != 1 {
		t.Fatal("0.17.2 closed filter")
	}
	raw, _ := s.db.Get(r.ID)
	var old oldReport
	json.Unmarshal(raw, &old)
	old.Status = "open"
	raw, _ = json.Marshal(old)
	s.db.Put(r.ID, raw)
	got, err := s.Get(r.ID)
	if err != nil || got.Resolution != "" || len(got.StatusChanges) != 0 {
		t.Fatal("old writer compatibility")
	}
}

func TestResolutionProjectPaginationAndStateFilters(t *testing.T) {
	s, _, a := resolutionFixture(t)
	puts := map[string][]byte{}
	for i := 0; i < 105; i++ {
		r := Report{Input: fixtureInput(), ID: fmt.Sprintf("bug-page-%04d", i), Reporter: reporter(1), Status: "closed", Resolution: "resolved", ReceivedAt: s.now()}
		if i >= 51 {
			r.Reporter.ProjectGenerationID = "BBBBBBBBBBBBBBBBBBBBBBBBBB"
		}
		raw, _ := json.Marshal(r)
		puts[r.ID] = raw
	}
	if err := s.db.Batch(puts, nil); err != nil {
		t.Fatal(err)
	}
	rows, total, err := s.ListProject(a.UserID, "project", reporter(1).ProjectGenerationID, "resolved", "", 2)
	if err != nil || total != 51 || len(rows) != 1 || rows[0].ExpectedVersion == "" {
		t.Fatal("scope/state pagination", total, len(rows), err)
	}
	rows, total, err = s.ListProject(a.UserID, "project", reporter(1).ProjectGenerationID, "closed", "", 1)
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatal("legacy closed inferred from resolved")
	}
	for _, tc := range []struct {
		state, severity string
		page            int
	}{{"bad", "", 1}, {"all", "bad", 1}, {"all", "", -1}} {
		if _, _, err = s.ListProject(a.UserID, "project", reporter(1).ProjectGenerationID, tc.state, tc.severity, tc.page); err != ErrInvalid {
			t.Fatal("invalid project filter")
		}
	}
}
func TestResolutionPolicyHeldUntilCommitAndFailureDoesNotWrite(t *testing.T) {
	s, r, a := resolutionFixture(t)
	releases := 0
	s.SetProjectPolicy(func(actor string) (map[string]string, func(), error) {
		if actor != "admin" {
			return nil, nil, ErrForbidden
		}
		return map[string]string{"project": reporter(1).ProjectGenerationID}, func() {
			releases++
			raw, e := s.db.Get(r.ID)
			var got Report
			if e != nil || json.Unmarshal(raw, &got) != nil || got.Resolution != "resolved" || len(got.StatusChanges) != 1 {
				t.Error("policy lease released before atomic commit")
			}
		}, nil
	})
	r, _, err := s.SetResolution(r.ID, "resolved", "", a, r.ExpectedVersion)
	if err != nil || releases != 1 {
		t.Fatal("policy lease", err)
	}
	// Closed storage refuses writes rather than returning a successful mutation.
	s.SetProjectPolicy(func(string) (map[string]string, func(), error) {
		return map[string]string{"project": reporter(1).ProjectGenerationID}, func() {}, nil
	})
	s.Close()
	if _, _, err = s.SetResolution(r.ID, "ignored", "", a, r.ExpectedVersion); err == nil {
		t.Fatal("closed database mutation succeeded")
	}
}

func TestPanelStateFiltersKeepCoarseLegacyRead(t *testing.T) {
	s, r, a := resolutionFixture(t)
	for _, state := range []string{"resolved", "ignored"} {
		next, err := s.Create(fixtureInput(), reporter(2), "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = s.SetResolution(next.ID, state, "", a, next.ExpectedVersion); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetStatus(r.ID, "closed", "admin"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"resolved", "ignored", "closed"} {
		rows, total, err := s.List(Filter{State: state}, 1)
		if err != nil || total != 1 || len(rows) != 1 || rows[0].State() != state {
			t.Fatal("precise state filter", state, total, err)
		}
	}
	_, total, err := s.List(Filter{Status: "closed"}, 1)
	if err != nil || total != 3 {
		t.Fatal("coarse reader changed")
	}
	if _, _, err = s.List(Filter{State: "invalid"}, 1); err != ErrInvalid {
		t.Fatal("bad precise filter")
	}
}
