package bugreport

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestGenerationRestoreBucketsAndFencedMutations(t *testing.T) {
	s, _ := fixtureStore(t)
	var mu sync.RWMutex
	current := map[string]string{"project": "AAAAAAAAAAAAAAAAAAAAAAAAAA"}
	s.SetProjectPolicy(func(actor string) (map[string]string, func(), error) {
		mu.RLock()
		if actor != "" && actor != "admin" {
			mu.RUnlock()
			return nil, nil, ErrForbidden
		}
		return current, mu.RUnlock, nil
	})
	old, err := s.Create(fixtureInput(), reporter(1), "test")
	if err != nil {
		t.Fatal(err)
	}
	backup, _ := s.db.Get(old.ID)
	mu.Lock()
	current = map[string]string{"project": "BBBBBBBBBBBBBBBBBBBBBBBBBB"}
	mu.Unlock()
	if err = s.DeleteProject("project", "AAAAAAAAAAAAAAAAAAAAAAAAAA"); err != nil {
		t.Fatal(err)
	}
	// Restore an older backup after same-name recreation. Do not infer its lifetime.
	if err = s.db.Put(old.ID, backup); err != nil {
		t.Fatal(err)
	}
	next := reporter(2)
	next.ProjectGenerationID = "BBBBBBBBBBBBBBBBBBBBBBBBBB"
	fresh, err := s.Create(fixtureInput(), next, "test")
	if err != nil {
		t.Fatal(err)
	}
	unknown := old
	unknown.ID = "bug-unknown"
	unknown.Reporter.ProjectGenerationID = ""
	raw, _ := json.Marshal(unknown)
	if err = s.db.Put(unknown.ID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Create(fixtureInput(), reporter(3), "test"); !errors.Is(err, ErrForbidden) {
		t.Fatal("stale-generation Create succeeded")
	}
	if _, err = s.Get(old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("old restored report is current")
	}
	if err = s.SetStatus(old.ID, "closed", "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatal("old-generation status mutated")
	}
	for bucket, want := range map[string]int{"current": 1, "previous": 1, "unknown": 1, "history": 2} {
		rows, total, err := s.ListFor("admin", Filter{Bucket: bucket}, 1)
		if err != nil || total != want || len(rows) != want {
			t.Fatalf("%s count/page incorrect", bucket)
		}
	}
	for _, actor := range []string{"owner", "developer", "viewer", ""} {
		if _, _, err = s.ListFor(actor, Filter{Bucket: "history"}, 1); !errors.Is(err, ErrForbidden) {
			t.Fatal("history list is not admin-only")
		}
		if _, err = s.GetFor(actor, old.ID, "previous"); !errors.Is(err, ErrForbidden) {
			t.Fatal("history detail is not admin-only")
		}
	}
	if _, err = s.GetFor("admin", old.ID, "previous"); err != nil {
		t.Fatal("admin previous detail missing")
	}
	if _, err = s.GetFor("admin", unknown.ID, "unknown"); err != nil {
		t.Fatal("admin unknown detail missing")
	}
	if _, _, err = s.List(Filter{Bucket: "unknown"}, 1); !errors.Is(err, ErrForbidden) {
		t.Fatal("current API exposes unknown")
	}
	if err = s.DeleteProject("project", "AAAAAAAAAAAAAAAAAAAAAAAAAA"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(fresh.ID); err != nil {
		t.Fatal("old-generation delete erased new report")
	}
	if _, err = s.GetFor("admin", unknown.ID, "unknown"); err != nil {
		t.Fatal("project delete erased unknown report")
	}
}

func TestGenerationFilterPrecedesCountAndPagination(t *testing.T) {
	s, now := fixtureStore(t)
	s.SetProjectPolicy(func(actor string) (map[string]string, func(), error) {
		if actor != "" && actor != "admin" {
			return nil, nil, ErrForbidden
		}
		return map[string]string{"project": "AAAAAAAAAAAAAAAAAAAAAAAAAA"}, func() {}, nil
	})
	puts := map[string][]byte{}
	for i := 0; i < 110; i++ {
		r := Report{Input: fixtureInput(), ID: fmt.Sprintf("bug-page-%04d", i), Reporter: reporter(i), Status: "open", ReceivedAt: *now}
		if i >= 51 {
			r.Reporter.ProjectGenerationID = "BBBBBBBBBBBBBBBBBBBBBBBBBB"
		}
		raw, _ := json.Marshal(r)
		puts[r.ID] = raw
	}
	if err := s.db.Batch(puts, nil); err != nil {
		t.Fatal(err)
	}
	rows, total, err := s.List(Filter{}, 2)
	if err != nil || total != 51 || len(rows) != 1 {
		t.Fatal("pagination counted other generations")
	}
	rows, total, err = s.ListFor("admin", Filter{Bucket: "previous"}, 2)
	if err != nil || total != 59 || len(rows) != 9 {
		t.Fatal("history pagination used current count")
	}
}

func TestGenerationPolicyHeldUntilReportCommit(t *testing.T) {
	for _, operation := range []string{"create", "status", "delete"} {
		t.Run(operation, func(t *testing.T) {
			s, _ := fixtureStore(t)
			r, err := s.Create(fixtureInput(), reporter(1), "test")
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.RWMutex
			entered, proceed := make(chan struct{}), make(chan struct{})
			current := map[string]string{"project": "AAAAAAAAAAAAAAAAAAAAAAAAAA"}
			s.SetProjectPolicy(func(actor string) (map[string]string, func(), error) {
				mu.RLock()
				close(entered)
				<-proceed
				return current, func() {
					defer mu.RUnlock()
					payload, readErr := s.db.Get(r.ID)
					switch operation {
					case "status":
						var saved Report
						if readErr != nil || json.Unmarshal(payload, &saved) != nil || saved.Status != "closed" {
							t.Error("generation fence released before status commit")
						}
					case "delete":
						if readErr == nil {
							t.Error("generation fence released before delete commit")
						}
					case "create":
						records, e := s.db.List()
						count := 0
						for key := range records {
							if strings.HasPrefix(key, "bug-") {
								count++
							}
						}
						if e != nil || count != 2 {
							t.Error("generation fence released before create commit")
						}
					}
				}, nil
			})
			result := make(chan error, 1)
			go func() {
				switch operation {
				case "create":
					_, err = s.Create(fixtureInput(), reporter(2), "test")
				case "status":
					err = s.SetStatus(r.ID, "closed", "admin")
				case "delete":
					err = s.DeleteProject("project", "AAAAAAAAAAAAAAAAAAAAAAAAAA")
				}
				result <- err
			}()
			<-entered
			if mu.TryLock() {
				mu.Unlock()
				t.Fatal("generation check did not fence replacement")
			}
			replaced := make(chan struct{})
			go func() {
				mu.Lock()
				current = map[string]string{"project": "BBBBBBBBBBBBBBBBBBBBBBBBBB"}
				mu.Unlock()
				close(replaced)
			}()
			close(proceed)
			if err = <-result; err != nil {
				t.Fatal("fenced operation failed")
			}
			<-replaced
		})
	}
}
