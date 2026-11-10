package controlpanel

import (
	"math"
	"time"
)

type directRate struct {
	Tokens float64
	At     time.Time
}

// directRateError preserves the public error code while carrying the wait
// calculated under the same lock as the token bucket decision.
type directRateError struct{ wait time.Duration }

func (e *directRateError) Error() string { return errDirectRate.Error() }
func (e *directRateError) Unwrap() error { return errDirectRate }

func (s *Store) directRateWaitLocked(project string, parent SessionToken, source string, fresh bool) time.Duration {
	now := s.now()
	if s.directRates == nil {
		s.directRates = map[string]directRate{}
	}
	for k, b := range s.directRates {
		if now.Sub(b.At) > 10*time.Minute {
			delete(s.directRates, k)
		}
	}
	if len(s.directRates) > 4096 {
		return time.Minute
	}
	type limit struct {
		key         string
		rate, burst float64
	}
	limits := []limit{{"retry/" + parent.ID, 20, 20}}
	if fresh {
		burst := 10
		if parent.MaxAutoBindings > 0 {
			burst = min(10, max(2, parent.MaxAutoBindings))
		}
		limits = append(limits, limit{"parent/" + parent.ID, 30, float64(burst)}, limit{"project/" + project, 60, 60}, limit{"source/" + digest(source), 30, 30})
	}
	next := map[string]directRate{}
	var wait time.Duration
	for _, l := range limits {
		b, ok := s.directRates[l.key]
		if !ok {
			b = directRate{l.burst, now}
		}
		if now.After(b.At) {
			b.Tokens = min(l.burst, b.Tokens+now.Sub(b.At).Minutes()*l.rate)
		}
		b.At = now
		if b.Tokens < 1 {
			delay := time.Duration(math.Ceil((1 - b.Tokens) / l.rate * float64(time.Minute)))
			wait = max(wait, delay)
		}
		b.Tokens--
		next[l.key] = b
	}
	// A rejected request consumes no bucket. Use the longest limiting wait so
	// retrying at the advertised time satisfies every bucket, not just the first.
	if wait > 0 {
		return wait
	}
	for k, b := range next {
		s.directRates[k] = b
	}
	return 0
}
