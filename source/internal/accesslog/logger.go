package accesslog

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"os"
	"sync/atomic"
	"time"
)

const queueCapacity = 1024
const maxEventBytes = 8 << 10

type Logger struct {
	queue      chan Record
	stop, done chan struct{}
	stopping   atomic.Bool
	deadline   atomic.Pointer[time.Time]
	writer     *fileWriter
	counters   *totals
	started    string
}

func Open(dataDir string, c Config) (*Logger, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.File == "" {
		return nil, nil
	}
	counters := &totals{}
	writer, err := openWriter(dataDir, c, counters)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, invalid("permission")
		}
		return nil, initFailure(err)
	}
	return startLogger(writer), nil
}
func startLogger(writer *fileWriter) *Logger {
	l := &Logger{queue: make(chan Record, queueCapacity), stop: make(chan struct{}), done: make(chan struct{}), writer: writer, counters: writer.counters, started: serverdiag.ProcessStartedAt()}
	serverdiag.StartBackground(serverdiag.BackgroundStop, nil, l.run, func() {
		defer close(l.done)
		if err := l.writer.close(); err != nil {
			l.counters.writeErrors.Add(1)
		}
	})
	return l
}
func (l *Logger) Stats() Counters { return l.counters.snapshot() }
func (l *Logger) Submit(record Record) {
	if l.stopping.Load() {
		l.counters.dropped.Add(1)
		return
	}
	record.Type = "access"
	record.Method = Method(record.Method)
	record.Route = serverdiag.CanonicalRoute(record.Route)
	record.Peer = safeIP(record.Peer)
	record.Client = safeIP(record.Client)
	if record.ClientSource != "forwarded" {
		record.ClientSource = "peer"
	}
	if record.Route != "{unmatched}" && record.Route != "/api/{unmatched}" {
		record.PathLen = nil
	}
	record.ProcessStartedAt = l.started
	select {
	case l.queue <- record:
	default:
		l.counters.dropped.Add(1)
	}
}
func (l *Logger) write(record Record) {
	record.ProcessStartedAt = l.started
	success := false
	// Maintenance can change counters/encoded width. Re-encode after preparation;
	// only writer-owned counter changes require another size/rotation check.
	for attempt := 0; attempt < 3; attempt++ {
		record.Counters = l.Stats()
		line, err := json.Marshal(record)
		if err != nil || len(line)+1 > maxEventBytes {
			break
		}
		before := record.Counters
		if !l.writer.prepare(len(line)+1, l.writer.now()) {
			break
		}
		after := l.Stats()
		if before.WriteErrors != after.WriteErrors || before.RotationErrors != after.RotationErrors || before.PruneErrors != after.PruneErrors || before.Reopens != after.Reopens {
			continue
		}
		success = l.writer.writeData(append(line, '\n'), l.writer.now())
		break
	}
	if !success {
		l.counters.dropped.Add(1)
	}
	if record.diagnostic != nil {
		serverdiag.Emit(*record.diagnostic)
	}
}
func (l *Logger) expired() bool {
	deadline := l.deadline.Load()
	return deadline != nil && !time.Now().Before(*deadline)
}

func (l *Logger) run() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastStatus := l.writer.now()
	lastCounters := l.Stats()
	status := func() {
		current := l.Stats()
		if current == lastCounters {
			return
		}
		l.write(Record{Type: "access_log_status", ProcessStartedAt: l.started})
		serverdiag.Emit(serverdiag.Event{Type: "access_log_status", AccessDropped: current.Dropped, WriteErrors: current.WriteErrors, RotationErrors: current.RotationErrors, PruneErrors: current.PruneErrors, Reopens: current.Reopens})
		lastCounters = current
	}
	for {
		select {
		case record := <-l.queue:
			l.write(record)
		case now := <-tick.C:
			l.writer.maintenance(now)
			if now.Sub(lastStatus) >= time.Minute {
				status()
				lastStatus = now
			}
		case <-l.stop:
			for {
				if l.expired() {
					l.counters.dropped.Add(uint64(len(l.queue)))
					status()
					return
				}
				select {
				case record := <-l.queue:
					l.write(record)
				default:
					status()
					return
				}
			}
		}
	}
}

// Close never closes the producer channel and never waits beyond the shared
// file/stderr drain deadline. A stuck OS write can outlive this deadline.
func (l *Logger) Close(ctx context.Context) {
	if l == nil {
		return
	}
	if l.stopping.CompareAndSwap(false, true) {
		deadline := time.Now().Add(2 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		l.deadline.Store(&deadline)
		close(l.stop)
	}
	deadline := l.deadline.Load()
	remaining := 2 * time.Second
	if deadline != nil {
		remaining = time.Until(*deadline)
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-l.done:
	case <-ctx.Done():
	case <-timer.C:
	}
}
