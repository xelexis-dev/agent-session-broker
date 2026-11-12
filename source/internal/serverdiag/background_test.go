package serverdiag

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type backgroundPanicWriter struct{}

func (backgroundPanicWriter) Write([]byte) (int, error) { panic("fixture-background-panic-secret") }

func TestBackgroundPanicWholeProcessPolicies(t *testing.T) {
	if mode := os.Getenv("ASB_BACKGROUND_PANIC_CHILD"); mode != "" {
		switch mode {
		case "restart":
			var attempts atomic.Int32
			done := make(chan struct{})
			StartBackground(BackgroundRestart, nil, func() {
				if attempts.Add(1) == 1 {
					panic("fixture-background-panic-secret")
				}
			}, func() { close(done) })
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("periodic task did not restart")
			}
			if attempts.Load() != 2 {
				t.Fatal("unexpected restart count")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			Shutdown(ctx)
			return
		case "stop":
			StartBackground(BackgroundStop, nil, func() { panic("fixture-background-panic-secret") }, nil)
		case "callback":
			BackgroundCallback(func() { panic("fixture-background-panic-secret") })()
		case "callback-error":
			_ = BackgroundErrorCallback(func() error { panic("fixture-background-panic-secret") })()
		case "diagnostic", "diagnostic-blocked", "diagnostic-double-panic":
			if mode == "diagnostic-blocked" {
				originalStderr = blockedWriter{make(chan struct{}, 1), make(chan struct{})}
			}
			if mode == "diagnostic-double-panic" {
				originalStderr = backgroundPanicWriter{}
			}
			Use(New(backgroundPanicWriter{}))
			Emit(Event{Type: "server-listening"})
		default:
			t.Fatal("unknown child mode")
		}
		time.Sleep(5 * time.Second)
		t.Fatal("fatal background policy did not exit")
	}
	for _, mode := range []string{"restart", "stop", "callback", "callback-error", "diagnostic", "diagnostic-blocked", "diagnostic-double-panic"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBackgroundPanicWholeProcessPolicies$", "-test.count=1")
			cmd.Env = append(os.Environ(), "ASB_BACKGROUND_PANIC_CHILD="+mode)
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("background policy exceeded subprocess deadline")
			}
			if bytes.Contains(output, []byte("fixture-background-panic-secret")) || bytes.Contains(output, []byte("--- FAIL:")) || bytes.Contains(output, []byte("panic: ")) {
				t.Fatal("panic leaked or child assertion failed")
			}
			if mode == "restart" {
				if err != nil || !bytes.Contains(output, []byte(`"restart_count":1`)) {
					t.Fatal("periodic child did not survive and report restart")
				}
			} else {
				var exited *exec.ExitError
				if !errors.As(err, &exited) || exited.ExitCode() != 1 {
					t.Fatal("stop child did not exit with code1")
				}
			}
			if mode != "diagnostic-blocked" && mode != "diagnostic-double-panic" {
				if !bytes.Contains(output, []byte(`"type":"server_background_panic"`)) || !bytes.Contains(output, []byte(`"panic_type":"string"`)) || !bytes.Contains(output, []byte(`"stack":[`)) {
					t.Fatal("sanitized background panic classification missing")
				}
				if mode != "restart" && (!bytes.Contains(output, []byte(`"type":"server-failed"`)) || !bytes.Contains(output, []byte(`"reason":"background_panic"`))) {
					t.Fatal("fixed fatal reason missing")
				}
			}
		})
	}
}

func TestBackgroundCallbacksPreserveReturns(t *testing.T) {
	var calls atomic.Int32
	var wg sync.WaitGroup
	wg.Go(BackgroundCallback(func() { calls.Add(1) }))
	wg.Wait()
	done := make(chan struct{})
	timer := time.AfterFunc(0, BackgroundCallback(func() { calls.Add(1); close(done) }))
	defer timer.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("guarded timer did not run")
	}
	sentinel := errors.New("ordinary error")
	err := BackgroundErrorCallback(func() error { return sentinel })()
	if calls.Load() != 2 || err != sentinel {
		t.Fatal("callback boundary changed normal execution or error identity")
	}
}

func TestBackgroundRestartCancellationCompletesOnce(t *testing.T) {
	stop := make(chan struct{})
	entered := make(chan struct{})
	done := make(chan struct{})
	var calls, finished atomic.Int32
	StartBackground(BackgroundRestart, stop, func() { calls.Add(1); close(entered); panic("fixture-background-panic-secret") }, func() { finished.Add(1); close(done) })
	<-entered
	close(stop)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("backoff ignored cancellation")
	}
	if calls.Load() != 1 || finished.Load() != 1 {
		t.Fatal("restart completion changed lifetime")
	}
	// A task stopped before launch must still release its completion accounting.
	StartBackground(BackgroundRestart, stop, func() { t.Error("cancelled work ran") }, func() { finished.Add(1) })
	deadline := time.After(time.Second)
	for finished.Load() != 2 {
		select {
		case <-deadline:
			t.Fatal("cancelled launch did not finish")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestBackgroundFatalSharesDrainDeadline(t *testing.T) {
	savedExit := backgroundExit
	backgroundFatalOnce = sync.Once{}
	var exits atomic.Int32
	backgroundExit = func(code int) {
		if code != 1 {
			t.Error("wrong exit code")
		}
		exits.Add(1)
	}
	defer func() { backgroundExit = savedExit; backgroundFatalOnce = sync.Once{} }()
	w := blockedWriter{make(chan struct{}, 1), make(chan struct{})}
	sink := New(w)
	restore := Use(sink)
	defer restore()
	defer func() {
		close(w.release)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		sink.Close(ctx)
	}()
	Emit(Event{Type: "server-listening"})
	<-w.entered
	var drained bool
	unregister := RegisterBackgroundDrain(func(ctx context.Context) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Second {
			t.Error("access drain has no shared bounded deadline")
		}
		<-ctx.Done()
		drained = true
	})
	defer unregister()
	started := time.Now()
	event := Panic("fixture-background-panic-secret")
	event.Type = "server_background_panic"
	fatalBackground(event)
	if !drained || exits.Load() != 1 || time.Since(started) > 3*time.Second {
		t.Fatal("fatal path exceeded shared drain budget or missed exit seam")
	}
}
