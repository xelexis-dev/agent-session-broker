package accesslog

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"errors"
	"syscall"
)

type panicWriteBlockedClose struct{ logHandle }

func (panicWriteBlockedClose) Write([]byte) (int, error) { panic("fixture-file-worker-panic-secret") }
func (panicWriteBlockedClose) Sync() error               { select {} }

func TestAccessLogPanicSkipsUnsafeFileCleanup(t *testing.T) {
	if root := os.Getenv("ASB_FILE_PANIC_CHILD_ROOT"); root != "" {
		cfg := Defaults()
		cfg.File = "logs/access.log"
		w, err := openWriter(root, cfg, &totals{})
		if err != nil {
			t.Fatal("file panic fixture failed")
		}
		w.file = panicWriteBlockedClose{w.file}
		logger := startLogger(w)
		defer serverdiag.RegisterBackgroundDrain(logger.Close)()
		logger.Submit(Record{Method: "GET", Route: "/test"})
		time.Sleep(5 * time.Second)
		t.Fatal("file worker panic did not exit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAccessLogPanicSkipsUnsafeFileCleanup$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ASB_FILE_PANIC_CHILD_ROOT="+t.TempDir())
	output, err := cmd.CombinedOutput()
	var exited *exec.ExitError
	if ctx.Err() != nil || !errors.As(err, &exited) || exited.ExitCode() != 1 || bytes.Contains(output, []byte("--- FAIL:")) {
		t.Fatal("file worker panic blocked in unsafe cleanup instead of exiting")
	}
	if bytes.Contains(output, []byte("fixture-file-worker-panic-secret")) || !bytes.Contains(output, []byte(`"reason":"background_panic"`)) {
		t.Fatal("file worker panic leaked or fixed reason missing")
	}
}

func testWriter(t *testing.T) (*fileWriter, string) {
	t.Helper()
	root := t.TempDir()
	c := Defaults()
	c.File = "logs/access.log"
	c.MaxSizeMiB = 1
	c.MaxFiles = 2
	c.MaxAgeDays = 1
	w, err := openWriter(root, c, &totals{})
	if err != nil {
		t.Fatal("writer preflight failed")
	}
	t.Cleanup(func() { w.close() })
	return w, filepath.Join(root, "logs")
}
func TestAccessLogRotationNoOverwriteAndPermissions(t *testing.T) {
	w, dir := testWriter(t)
	now := time.Now().UTC()
	target := w.base + "." + now.Format("20060102T150405.000000000Z") + "-000001.rot"
	if os.WriteFile(filepath.Join(dir, target), []byte("existing"), 0600) != nil {
		t.Fatal("collision fixture failed")
	}
	if !w.write([]byte("first\n"), now) || w.rotate(now) != nil {
		t.Fatal("rotation failed")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, target))
	if string(raw) != "existing" {
		t.Fatal("rotation overwrote collision")
	}
	if runtime.GOOS != "windows" {
		for _, name := range []string{dir, filepath.Join(dir, "access.log")} {
			info, _ := os.Stat(name)
			if info.Mode().Perm()&0077 != 0 {
				t.Fatal("log permissions widened")
			}
		}
	}
}
func TestAccessLogRenameFailureGrowthCapAndRecovery(t *testing.T) {
	w, _ := testWriter(t)
	now := time.Now()
	original := w.rename
	file := w.file.(*os.File)
	if file.Truncate(MiB) != nil {
		t.Fatal("size fixture failed")
	}
	w.size = MiB
	w.rename = func(string, string) error { return os.ErrPermission }
	line := []byte(strings.Repeat("x", 8<<10))
	for w.size+int64(len(line)) <= 2*MiB {
		if !w.write(line, now) {
			t.Fatal("bounded fallback append stopped early")
		}
	}
	if w.write(line, now) || w.size > 2*MiB {
		t.Fatal("rename failure growth unbounded")
	}
	if w.counters.rotationErrors.Load() == 0 {
		t.Fatal("rotation failure uncounted")
	}
	w.rename = original
	w.maintenance(now.Add(31 * time.Second))
	if !w.write([]byte("recovered\n"), now.Add(31*time.Second)) {
		t.Fatal("rotation recovery failed")
	}
}
func TestAccessLogPruneClockAgeCountAndForeignFiles(t *testing.T) {
	w, dir := testWriter(t)
	now := time.Now().UTC()
	names := []string{"unrelated.rot", "other.log.20200101T000000.000000000Z-000001.rot", ".access.log.asb-access-preflight-NOTHEX.tmp"}
	for _, name := range names {
		if os.WriteFile(filepath.Join(dir, name), nil, 0600) != nil {
			t.Fatal("foreign fixture failed")
		}
	}
	for i, at := range []time.Time{now.Add(-48 * time.Hour), now.Add(-24 * time.Hour), now.Add(time.Hour), now.Add(2 * time.Hour)} {
		name := w.base + "." + at.Format("20060102T150405.000000000Z") + "-" + sequenceName(i+1) + ".rot"
		if os.WriteFile(filepath.Join(dir, name), nil, 0600) != nil {
			t.Fatal("rotation fixture failed")
		}
	}
	w.prune(now)
	entries, _ := os.ReadDir(dir)
	count := 0
	for _, e := range entries {
		if w.rotated.MatchString(e.Name()) {
			count++
		}
	}
	if count != 2 {
		t.Fatal("age/count pruning failed")
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal("foreign file removed")
		}
	}
	w.prune(now.Add(-100 * time.Hour))
	entries, _ = os.ReadDir(dir)
	for _, e := range entries {
		if w.rotated.MatchString(e.Name()) {
			count--
		}
	}
	if count != 0 {
		t.Fatal("clock rollback removed future rotations")
	}
}
func TestAccessLogReservedTempAndSymlinkBoundaries(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "logs")
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("fixture failed")
	}
	stale := ".access.log.asb-access-preflight-" + strings.Repeat("a", 32) + ".tmp"
	foreign := ".other.log.asb-access-preflight-" + strings.Repeat("a", 32) + ".tmp"
	for _, name := range []string{stale, foreign} {
		if os.WriteFile(filepath.Join(dir, name), nil, 0600) != nil {
			t.Fatal("fixture failed")
		}
	}
	c := Defaults()
	c.File = "logs/access.log"
	l, err := Open(root, c)
	if err != nil {
		t.Fatal("preflight failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l.Close(ctx)
	if _, err = os.Lstat(filepath.Join(dir, stale)); !os.IsNotExist(err) {
		t.Fatal("stale matching temp retained")
	}
	if _, err = os.Lstat(filepath.Join(dir, foreign)); err != nil {
		t.Fatal("foreign temp removed")
	}
	for _, path := range []string{"../outside.log", "control/db", "logs"} {
		c.File = path
		if _, err = Open(root, c); err == nil {
			t.Fatal("invalid log boundary accepted")
		}
	}
}
func TestAccessLogSameFileReplacementAndShortWriteRetry(t *testing.T) {
	w, dir := testWriter(t)
	now := time.Now()
	if !w.write([]byte("old\n"), now) {
		t.Fatal("initial write failed")
	}
	if os.Rename(filepath.Join(dir, w.base), filepath.Join(dir, "moved.log")) != nil {
		t.Fatal("replacement rename failed")
	}
	if os.WriteFile(filepath.Join(dir, w.base), []byte("new"), 0600) != nil {
		t.Fatal("replacement fixture failed")
	}
	w.maintenance(now.Add(time.Minute))
	if w.counters.reopens.Load() != 1 {
		t.Fatal("SameFile mismatch was not reopened")
	}
	actual := w.file
	w.file = shortHandle{actual}
	if w.write([]byte("partial\n"), now.Add(time.Minute)) {
		t.Fatal("short write accepted")
	}
	if w.counters.writeErrors.Load() != 1 {
		t.Fatal("write error uncounted")
	}
	if w.write([]byte("too-soon\n"), now.Add(time.Minute)) {
		t.Fatal("retry backoff ignored")
	}
	w.maintenance(now.Add(time.Minute + 2*time.Second))
	if !w.write([]byte("recovered\n"), now.Add(time.Minute+2*time.Second)) {
		t.Fatal("idle retry did not recover")
	}
}

type shortHandle struct{ logHandle }

func (shortHandle) Write(p []byte) (int, error) { return 1, io.ErrShortWrite }

type blockingHandle struct {
	logHandle
	entered, release chan struct{}
}

func (b blockingHandle) Write(p []byte) (int, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return b.logHandle.Write(p)
}
func TestAccessLogBlockedSinkQueueAndLateProducer(t *testing.T) {
	w, _ := testWriter(t)
	blocked := blockingHandle{w.file, make(chan struct{}, 1), make(chan struct{})}
	w.file = blocked
	l := startLogger(w)
	l.Submit(Record{Method: "GET", Route: "/test"})
	<-blocked.entered
	for range 2048 {
		l.Submit(Record{Method: "GET", Route: "/test"})
	}
	if len(l.queue) != queueCapacity || l.Stats().Dropped == 0 {
		t.Fatal("queue not bounded or losses uncounted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l.Close(ctx)
	before := l.Stats().Dropped
	l.Submit(Record{Method: "GET", Route: "/test"})
	if l.Stats().Dropped <= before {
		t.Fatal("late producer was not dropped")
	}
	close(blocked.release)
	select {
	case <-l.done:
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not stop")
	}
}
func TestAccessLogOffAndWideModeRejection(t *testing.T) {
	root := t.TempDir()
	if l, err := Open(root, Defaults()); err != nil || l != nil {
		t.Fatal("default logging enabled")
	}
	if _, err := os.Lstat(filepath.Join(root, "logs")); !os.IsNotExist(err) {
		t.Fatal("off created files")
	}
	if runtime.GOOS != "windows" {
		if os.Mkdir(filepath.Join(root, "logs"), 0755) != nil {
			t.Fatal("fixture failed")
		}
		c := Defaults()
		c.File = "logs/access.log"
		if _, err := Open(root, c); err == nil {
			t.Fatal("wide log directory accepted")
		}
	}
}
func BenchmarkAccessLogEncoder(b *testing.B) {
	event := Record{Type: "access", Method: "GET", Route: "/healthz", Status: 200, Client: "127.0.0.1", Peer: "127.0.0.1", ClientSource: "peer", ProcessStartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := json.Marshal(event); err != nil {
			b.Fatal("encoding failed")
		}
	}
}
func BenchmarkAccessLogBlockedSink(b *testing.B) {
	root := b.TempDir()
	c := Defaults()
	c.File = "logs/access.log"
	w, err := openWriter(root, c, &totals{})
	if err != nil {
		b.Fatal("open failed")
	}
	blocked := blockingHandle{w.file, make(chan struct{}, 1), make(chan struct{})}
	w.file = blocked
	l := startLogger(w)
	l.Submit(Record{Method: "GET", Route: "/test"})
	<-blocked.entered
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		l.Submit(Record{Method: "GET", Route: "/test"})
	}
	b.StopTimer()
	close(blocked.release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	l.Close(ctx)
}

func TestAccessLogDirectoryReplacementFailsClosed(t *testing.T) {
	w, dir := testWriter(t)
	now := time.Now()
	if !w.write([]byte("before\n"), now) {
		t.Fatal("fixture write failed")
	}
	moved := dir + "-moved"
	err := os.Rename(dir, moved)
	if runtime.GOOS == "windows" {
		if err == nil {
			t.Fatal("pinned directory unexpectedly renamed")
		}
		if !errors.Is(err, os.ErrPermission) && !errors.Is(err, syscall.Errno(32)) {
			t.Fatal("unexpected directory rename failure")
		}
		return
	}
	if err != nil || os.Mkdir(dir, 0700) != nil {
		t.Fatal("directory replacement fixture failed")
	}
	if w.write([]byte("after\n"), now) {
		t.Fatal("writer continued in detached log directory")
	}
	w.maintenance(now.Add(time.Minute))
	if w.file != nil {
		t.Fatal("detached directory reopened")
	}
	raw, err := os.ReadFile(filepath.Join(moved, w.base))
	if err != nil || strings.Contains(string(raw), "after") {
		t.Fatal("detached file changed")
	}
}

type noSpaceHandle struct{ logHandle }

func (noSpaceHandle) Write([]byte) (int, error) { return 0, syscall.ENOSPC }

func TestAccessLogENOSPCAndReopenDenial(t *testing.T) {
	w, dir := testWriter(t)
	now := time.Now()
	w.now = func() time.Time { return now }
	w.file = noSpaceHandle{w.file}
	l := &Logger{writer: w, counters: w.counters, started: "fixture-process"}
	event := Record{Type: "access", Method: "GET", Route: "/test"}
	l.write(event)
	if w.file != nil || l.Stats().WriteErrors != 1 || l.Stats().Dropped != 1 {
		t.Fatal("ENOSPC did not close the handle and count the lost record")
	}
	open := w.open
	attempts := 0
	w.open = func(string, bool) (*os.File, error) {
		attempts++
		return nil, os.ErrPermission
	}
	w.maintenance(now)
	if attempts != 0 {
		t.Fatal("ENOSPC retry ignored backoff")
	}
	now = now.Add(time.Second)
	w.maintenance(now)
	if attempts != 1 || w.file != nil || l.Stats().WriteErrors != 2 {
		t.Fatal("reopen denial was not counted and kept closed")
	}
	l.write(event)
	if attempts != 1 || l.Stats().Dropped != 2 {
		t.Fatal("record during reopen backoff blocked or retried early")
	}
	w.open = open
	now = now.Add(2 * time.Second)
	w.maintenance(now)
	l.write(event)
	if w.file == nil || l.Stats().Reopens != 1 || l.Stats().Dropped != 2 {
		t.Fatal("idle retry failed to recover after permission restoration")
	}
	raw, err := os.ReadFile(filepath.Join(dir, w.base))
	if err != nil {
		t.Fatal("recovery file missing")
	}
	var recovered Record
	if json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &recovered) != nil || recovered.WriteErrors != 2 || recovered.Dropped != 2 || recovered.Reopens != 1 || recovered.ProcessStartedAt != "fixture-process" {
		t.Fatal("first recovered row omitted current failure counters")
	}
}

func TestAccessLogHTTPHandlerFinishesDuringDrain(t *testing.T) {
	w, _ := testWriter(t)
	blocked := blockingHandle{w.file, make(chan struct{}, 1), make(chan struct{})}
	w.file = blocked
	l := startLogger(w)
	defer func() {
		close(blocked.release)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		l.Close(ctx)
		select {
		case <-l.done:
		case <-ctx.Done():
			t.Error("file worker did not finish after sink release")
		}
	}()
	l.Submit(Record{Method: "GET", Route: "/test"})
	select {
	case <-blocked.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("file worker did not enter blocked sink")
	}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseHandler()
	handler := l.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}), testMeta)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		handler.ServeHTTP(w, r)
	}))
	defer func() { releaseHandler(); server.Close() }()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		response, err := server.Client().Get(server.URL)
		if err == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP handler did not start")
	}
	// Exercise Run's five-second HTTP shutdown before the shared two-second drain.
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := server.Config.Shutdown(shutdown)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("blocked handler did not outlive the HTTP shutdown deadline")
	}
	select {
	case <-finished:
		t.Fatal("blocked handler finished before the drain")
	default:
	}
	_ = server.Config.Close()
	drain, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	closed := make(chan struct{})
	go func() { l.Close(drain); close(closed) }()
	for !l.stopping.Load() {
		select {
		case <-drain.Done():
			t.Fatal("drain never entered stopping state")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	before := l.Stats().Dropped
	releaseHandler()
	select {
	case <-finished:
	case <-drain.Done():
		t.Fatal("late HTTP completion blocked in the drain")
	}
	if l.Stats().Dropped != before+1 {
		t.Fatal("late HTTP access event was not counted as a drop")
	}
	select {
	case <-closed:
		t.Fatal("blocked sink returned before its drain deadline")
	default:
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("drain exceeded its bounded deadline")
	}
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatal("closed HTTP connection retained client goroutine")
	}
}
