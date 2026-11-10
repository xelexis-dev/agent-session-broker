package channel

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"
)

// Only direct discovery/exchange waits are instant. Device polling retains its
// own clock so pending authorization cannot spin without a server transition.
type recordingAuthClock struct {
	mu     sync.Mutex
	delays []time.Duration
}

func installRecordingAuthClock(c *deviceAuthClient) *recordingAuthClock {
	clock := &recordingAuthClock{}
	c.clock = devicePollClock{now: time.Now, sleep: func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		clock.mu.Lock()
		clock.delays = append(clock.delays, d)
		clock.mu.Unlock()
		return nil
	}}
	return clock
}

func newInstantDeviceAuthClient(endpoint, token string, client *http.Client) *deviceAuthClient {
	c := newDeviceAuthClient(endpoint, token, client)
	installRecordingAuthClock(c)
	return c
}

func (c *recordingAuthClock) assertDelays(t *testing.T, want ...time.Duration) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Equal(c.delays, want) {
		t.Fatalf("retry delays = %v, want %v", c.delays, want)
	}
}
