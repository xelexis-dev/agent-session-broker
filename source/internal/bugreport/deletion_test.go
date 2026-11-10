package bugreport

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type failingReportDB struct {
	reportDatabase
	failDelete           bool
	failBatchAt, batches int
}

func (d *failingReportDB) Delete(id string) error {
	if d.failDelete {
		return errors.New("injected delete failure")
	}
	return d.reportDatabase.Delete(id)
}
func (d *failingReportDB) Batch(puts map[string][]byte, deletes []string) error {
	d.batches++
	if d.failBatchAt == d.batches {
		return errors.New("injected record failure")
	}
	return d.reportDatabase.Batch(puts, deletes)
}
func deletionFixture(t *testing.T) (*Store, *time.Time, Report) {
	t.Helper()
	s, now := fixtureStore(t)
	r, err := s.Create(Input{Title: "DELETE-SENTINEL", Summary: "DELETE-SENTINEL"}, reporter(1), "test")
	if err != nil {
		t.Fatal(err)
	}
	s.SetProjectPolicy(func(actor string) (map[string]string, func(), error) {
		if actor != "admin" && actor != "" {
			return nil, nil, ErrForbidden
		}
		return map[string]string{"project": reporter(1).ProjectGenerationID}, func() {}, nil
	})
	return s, now, r
}
func TestDeletionRemovesContentKeepsQuotaAndLegacyRows(t *testing.T) {
	s, _, r := deletionFixture(t)
	if _, err := s.Create(fixtureInput(), reporter(2), "test"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.db.List()
	d, err := s.Delete("admin", r.ID, r.ExpectedVersion)
	if err != nil || d.Phase != "deleted" {
		t.Fatal(d, err)
	}
	if _, err = s.Get(r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	after, err := s.db.List()
	if err != nil {
		t.Fatal(err)
	}
	for key, raw := range after {
		if strings.HasPrefix(key, "quota-") {
			if string(raw) != string(before[key]) {
				t.Fatal("quota changed")
			}
			continue
		}
		// Released 0.17.2 rowsLocked treats every non-quota row as a report.
		var old struct {
			ID     string
			Status string
		}
		if !strings.HasPrefix(key, "bug-") || json.Unmarshal(raw, &old) != nil || old.ID != key || !validStatus(old.Status) {
			t.Fatal("0.17.2 rows invalid")
		}
	}
	rows, err := s.deletions.List()
	if err != nil || len(rows) != 1 {
		t.Fatal("records", err)
	}
	var fields map[string]any
	if json.Unmarshal(rows[r.ID], &fields) != nil || len(fields) != 10 || strings.Contains(string(rows[r.ID]), "DELETE-SENTINEL") {
		t.Fatal("record leaked report fields")
	}
	for _, key := range []string{"title", "summary", "note", "sessionName", "registrationId", "project", "client", "expectedVersion"} {
		if _, ok := fields[key]; ok {
			t.Fatal("disallowed field", key)
		}
	}
	if got, err := s.DeletionFor("admin", r.ID); err != nil || got != d {
		t.Fatal(got, err)
	}
}
func TestDeletionFailurePhasesRetryAndPruning(t *testing.T) {
	for _, mode := range []string{"delete-retry", "delete-prune", "complete"} {
		t.Run(mode, func(t *testing.T) {
			s, now, r := deletionFixture(t)
			if _, err := s.deletionDBLocked(); err != nil {
				t.Fatal(err)
			}
			if mode == "complete" {
				s.deletions = &failingReportDB{reportDatabase: s.deletions, failBatchAt: 2}
			} else {
				s.db = &failingReportDB{reportDatabase: s.db, failDelete: true}
			}
			_, err := s.Delete("admin", r.ID, r.ExpectedVersion)
			if mode == "complete" {
				if !errors.Is(err, ErrDeleteIncomplete) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
			d, err := s.DeletionFor("admin", r.ID)
			if err != nil || d.Phase != "requested" {
				t.Fatal(d, err)
			}
			_, getErr := s.Get(r.ID)
			if mode == "complete" {
				if !errors.Is(getErr, ErrNotFound) {
					t.Fatal(getErr)
				}
				return
			}
			if getErr != nil {
				t.Fatal("report disappeared after failed delete", getErr)
			}
			s.db.(*failingReportDB).failDelete = false
			if mode == "delete-prune" {
				*now = now.Add(Retention - time.Hour)
				// An earlier report expires while its later deletion request is still retained.
				r.ReceivedAt = r.ReceivedAt.Add(-2 * time.Hour)
				raw, _ := json.Marshal(r)
				s.db.Put(r.ID, raw)
				if _, err := s.Create(fixtureInput(), reporter(2), "test"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Get(r.ID); !errors.Is(err, ErrNotFound) {
					t.Fatal(err)
				}
				d, err = s.DeletionFor("admin", r.ID)
				if err != nil || d.Phase != "requested" {
					t.Fatal("retention became confirmed deletion", d, err)
				}
			} else {
				if _, err := s.Delete("admin", r.ID, r.ExpectedVersion); err != nil {
					t.Fatal(err)
				}
				rows, _ := s.deletions.List()
				d, _ := s.DeletionFor("admin", r.ID)
				if len(rows) != 1 || d.Phase != "deleted" {
					t.Fatal("retry duplicated/lost record")
				}
			}
		})
	}
}
func TestDeletionCASPermissionAndUnavailableHaveNoSideEffects(t *testing.T) {
	s, _, r := deletionFixture(t)
	if _, err := s.Delete("other", r.ID, r.ExpectedVersion); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := s.Delete("admin", r.ID, "wrong"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if s.deletions != nil {
		t.Fatal("CAS opened deletion DB")
	}
	if err := os.Mkdir(filepath.Join(s.directory, "deletions.db"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete("admin", r.ID, r.ExpectedVersion); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := s.Get(r.ID); err != nil {
		t.Fatal("unavailable deleted report", err)
	}
	if _, err := s.DeletionFor("other", r.ID); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
}
func TestDeletionCompetesWithDeleteAndStatus(t *testing.T) {
	for _, status := range []bool{false, true} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s, _, r := deletionFixture(t)
			start := make(chan struct{})
			errs := make(chan error, 2)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); <-start; _, err := s.Delete("admin", r.ID, r.ExpectedVersion); errs <- err }()
			go func() {
				defer wg.Done()
				<-start
				var err error
				if status {
					_, _, err = s.SetResolution(r.ID, "resolved", "", Actor{UserID: "admin", Source: "panel"}, r.ExpectedVersion)
				} else {
					_, err = s.Delete("admin", r.ID, r.ExpectedVersion)
				}
				errs <- err
			}()
			close(start)
			wg.Wait()
			close(errs)
			success := 0
			for err := range errs {
				if err == nil {
					success++
				} else if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
					t.Fatal(err)
				}
			}
			if success != 1 {
				t.Fatal("multiple mutations committed", success)
			}
		})
	}
}
func TestDeletionRetentionCapAndProjectRemoval(t *testing.T) {
	s, now, r := deletionFixture(t)
	if _, err := s.deletionDBLocked(); err != nil {
		t.Fatal(err)
	}
	puts := map[string][]byte{}
	for i := 0; i < MaxReports; i++ {
		d := Deletion{Version: 1, ReportID: fmt.Sprintf("bug-old-%04d", i), Phase: "deleted", DeletedAt: now.Add(-time.Duration(i+1) * time.Minute), Actor: "admin", Source: "panel", ProjectGenerationID: "other-generation", ReceivedAt: *now, Severity: "medium", State: "open"}
		raw, _ := json.Marshal(d)
		puts[d.ReportID] = raw
	}
	if err := s.deletions.Batch(puts, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete("admin", r.ID, r.ExpectedVersion); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.deletions.List()
	if len(rows) != MaxReports {
		t.Fatal("cap", len(rows))
	}
	if _, ok := rows["bug-old-1999"]; ok {
		t.Fatal("oldest retained")
	}
	if err := s.DeleteProject("project", r.Reporter.ProjectGenerationID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeletionFor("admin", r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	rows, _ = s.deletions.List()
	if len(rows) != MaxReports-1 {
		t.Fatal("other generation removed")
	}
	*now = now.Add(Retention)
	next, err := s.Create(fixtureInput(), reporter(2), "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete("admin", next.ID, next.ExpectedVersion); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.deletions.List()
	if len(rows) != 1 {
		t.Fatal("retention", len(rows))
	}
}
func TestDeletionOldGenerationAndProjectRecordFailure(t *testing.T) {
	s, _, r := deletionFixture(t)
	s.SetProjectPolicy(func(string) (map[string]string, func(), error) {
		return map[string]string{"project": "NEW-GENERATION"}, func() {}, nil
	})
	if _, err := s.Delete("admin", r.ID, r.ExpectedVersion); err != nil {
		t.Fatal("old generation delete", err)
	}
	unknown := r
	unknown.ID = "bug-unknown"
	unknown.Reporter.ProjectGenerationID = ""
	raw, _ := json.Marshal(unknown)
	if err := s.db.Put(unknown.ID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete("admin", unknown.ID, recordVersion(raw)); err != nil {
		t.Fatal("unknown generation", err)
	}
	s.deletions = &failingReportDB{reportDatabase: s.deletions, failBatchAt: 1}
	if err := s.DeleteProject("project", r.Reporter.ProjectGenerationID); err != nil {
		t.Fatal("metadata failure blocked project deletion", err)
	}
	if _, err := s.DeletionFor("admin", r.ID); err != nil {
		t.Fatal("failed delete lost record", err)
	}
}

func TestDeletionStoreCloseDoesNotOpenLazyDatabase(t *testing.T) {
	s, _, r := deletionFixture(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeletionFor("admin", r.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatal("closed store reopened deletion DB", err)
	}
	if s.deletions != nil {
		t.Fatal("closed store leaked database")
	}
}
