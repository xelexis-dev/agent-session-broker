package serverdiag

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

type BackgroundPolicy uint8

const (
	BackgroundStop BackgroundPolicy = iota
	BackgroundRestart
	backgroundDiagnostic
	backgroundFallback
)

var backgroundExit = os.Exit
var originalStderr io.Writer = os.Stderr
var backgroundFatalOnce sync.Once
var backgroundDrains = struct {
	sync.Mutex
	next  uint64
	hooks map[uint64]func(context.Context)
}{hooks: make(map[uint64]func(context.Context))}

// RegisterBackgroundDrain avoids a dependency from diagnostics to the file logger.
func RegisterBackgroundDrain(drain func(context.Context)) func() {
	backgroundDrains.Lock()
	backgroundDrains.next++
	id := backgroundDrains.next
	backgroundDrains.hooks[id] = drain
	backgroundDrains.Unlock()
	return func() { backgroundDrains.Lock(); delete(backgroundDrains.hooks, id); backgroundDrains.Unlock() }
}

// StartBackground owns the panic boundary and completion callback. Restart is
// only for idempotent work; finished runs once, after all restart attempts.
func StartBackground(policy BackgroundPolicy, stop <-chan struct{}, work, finished func()) {
	go runBackground(policy, stop, work, finished)
}

// BackgroundCallback guards a one-shot callback launched by another API.
func BackgroundCallback(work func()) func() {
	return func() { runBackground(BackgroundStop, nil, work, nil) }
}

// BackgroundErrorCallback preserves ordinary errors for errgroup-style APIs.
func BackgroundErrorCallback(work func() error) func() error {
	return func() error {
		var err error
		runBackground(BackgroundStop, nil, func() { err = work() }, nil)
		return err
	}
}

func backgroundPanic(work func()) (event *Event) {
	defer func() {
		if value := recover(); value != nil {
			e := Panic(value)
			e.Type = "server_background_panic"
			event = &e
		}
	}()
	work()
	return nil
}

func runBackground(policy BackgroundPolicy, stop <-chan struct{}, work, finished func()) {
	defer func() {
		if value := recover(); value != nil {
			e := Panic(value)
			e.Type = "server_background_panic"
			if policy == backgroundFallback {
				return
			}
			if policy == backgroundDiagnostic {
				diagnosticPanic(e)
				return
			}
			fatalBackground(e)
		}
	}()
	if finished != nil {
		defer finished()
	}
	backoff := time.Second
	var restarts uint64
	for {
		if policy == BackgroundRestart {
			select {
			case <-stop:
				return
			default:
			}
		}
		event := backgroundPanic(work)
		if event == nil {
			return
		}
		switch policy {
		case backgroundFallback:
			return
		case backgroundDiagnostic:
			diagnosticPanic(*event)
			return
		case BackgroundRestart:
			restarts++
			event.RestartCount = restarts
			Emit(*event)
			timer := time.NewTimer(backoff)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			if backoff < time.Minute {
				backoff *= 2
				if backoff > time.Minute {
					backoff = time.Minute
				}
			}
		default:
			fatalBackground(*event)
			return
		}
	}
}

func fatalBackground(event Event) {
	backgroundFatalOnce.Do(func() {
		defer func() { _ = recover(); backgroundExit(1) }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		Emit(event)
		Emit(Event{Type: "server-failed", Reason: "background_panic"})
		backgroundDrains.Lock()
		hooks := make([]func(context.Context), 0, len(backgroundDrains.hooks))
		for _, hook := range backgroundDrains.hooks {
			hooks = append(hooks, hook)
		}
		backgroundDrains.Unlock()
		for _, hook := range hooks {
			hook(ctx)
		}
		Shutdown(ctx)
	})
}

// The queue's own writer cannot enqueue a report about its failure. One guarded
// direct write to the original stderr gets one second, then the process exits.
func diagnosticPanic(event Event) {
	defer func() { _ = recover(); backgroundExit(1) }()
	event.ProcessStartedAt = processStartedAt
	first, _ := json.Marshal(event)
	last, _ := json.Marshal(Event{Type: "server-failed", Reason: "background_panic", ProcessStartedAt: processStartedAt})
	data := append(append(append(first, '\n'), last...), '\n')
	done := make(chan struct{})
	StartBackground(backgroundFallback, nil, func() { _, _ = originalStderr.Write(data) }, func() { close(done) })
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}
