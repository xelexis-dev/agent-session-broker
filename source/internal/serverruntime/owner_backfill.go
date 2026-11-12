package serverruntime

import (
	"context"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"sync"
	"time"
)

var ownerBackfillRetryDelay = 5 * time.Second

type ownerBackfiller interface{ BackfillRegistrationOwners(map[string]string) error }

// Convenience migration failures never change normal storage failure policy.
// Failed records remain owner-less; the service and Owner/admin management stay available.
type ownerBackfillRetries struct {
	mu      sync.Mutex
	stopped bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func newOwnerBackfillRetries() *ownerBackfillRetries {
	ctx, cancel := context.WithCancel(context.Background())
	return &ownerBackfillRetries{ctx: ctx, cancel: cancel}
}
func attemptOwnerBackfill(service ownerBackfiller, bindings map[string]string) bool {
	if err := service.BackfillRegistrationOwners(bindings); err != nil {
		count := 0
		if pending, ok := service.(interface{ PendingRegistrationOwnerBackfills(map[string]string) int }); ok {
			count = pending.PendingRegistrationOwnerBackfills(bindings)
		}
		serverdiag.Emit(serverdiag.Event{Type: "registration-owner-backfill-failed", Count: uint64(count)})
		return false
	}
	return true
}
func (r *ownerBackfillRetries) run(service ownerBackfiller, bindings map[string]string) {
	if attemptOwnerBackfill(service, bindings) {
		return
	}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		timer := time.NewTimer(ownerBackfillRetryDelay)
		defer timer.Stop()
		select {
		case <-r.ctx.Done():
			return
		case <-timer.C:
		}
		if r.ctx.Err() != nil {
			return
		}
		attemptOwnerBackfill(service, bindings) // Exactly one retry, even if it fails.
	}()
}
func (r *ownerBackfillRetries) close() {
	r.mu.Lock()
	r.stopped = true
	r.cancel()
	r.mu.Unlock()
	r.wg.Wait()
}
