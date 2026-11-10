package bugreport

import (
	"errors"
	"sync"
	"testing"
)

func TestMisuseD8ConcurrentReportsKeepQuotaAndRecordsAtomic(t *testing.T) {
	store, _ := fixtureStore(t)
	var group sync.WaitGroup
	success := make(chan string, 64)
	failures := make(chan error, 64)
	start := make(chan struct{})
	for i := 0; i < 64; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			report, err := store.Create(fixtureInput(), reporter(1), "test")
			if err == nil {
				success <- report.ID
			} else {
				failures <- err
			}
		}()
	}
	close(start)
	group.Wait()
	close(success)
	close(failures)
	ids := map[string]bool{}
	for id := range success {
		if ids[id] {
			t.Fatal("duplicate report")
		}
		ids[id] = true
	}
	if len(ids) != 10 {
		t.Fatal("concurrent reports bypassed hourly quota")
	}
	for err := range failures {
		if !errors.Is(err, ErrRateLimited) {
			t.Fatal("unexpected report failure")
		}
	}
	rows, total, err := store.List(Filter{}, 1)
	if err != nil || total != 10 || len(rows) != 10 {
		t.Fatal("quota and record state diverged")
	}
	for _, row := range rows {
		if !ids[row.ID] {
			t.Fatal("unaccepted report persisted")
		}
	}
}
