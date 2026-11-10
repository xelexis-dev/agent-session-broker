package localactivity

import (
	"context"
	"io"
	"strings"
	"testing"
)

type countingReaderAt struct {
	io.ReaderAt
	readBytes int
}

func (r *countingReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, offset)
	r.readBytes += n
	return n, err
}

func TestSessionMetaReadsOnlyBoundedLine(t *testing.T) {
	meta := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"synthetic\"}}"
	data := meta + "\n" + strings.Repeat("x", 2*metaLimit)
	reader := &countingReaderAt{ReaderAt: strings.NewReader(data)}
	budget := &readBudget{ctx: context.Background(), left: 1 << 20}
	got, err := budget.readSessionMeta(reader, int64(len(data)))
	if err != nil || string(got) != meta {
		t.Fatal("metadata changed", err)
	}
	if reader.readBytes > 1024 {
		t.Fatalf("metadata line read %d bytes, want at most 1024", reader.readBytes)
	}
	if budget.left != 1<<20-int64(reader.readBytes) {
		t.Fatal("logical read budget did not count actual bytes")
	}
}

func unchangedCodexFixture(t testing.TB) *fixture {
	f := newFixture(t, "codex")
	f.write(f.meta(sessionA) + f.event("task_started", "turn-a") + f.event("task_complete", "turn-a") +
		"{\"type\":\"turn_context\",\"payload\":{\"model\":\"gpt-6.1-sol\",\"effort\":\"medium\"}}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"content\":\"" + strings.Repeat("x", 100<<10) + "\"}}\n")
	if got := f.sample(); got.State != Idle || got.Model != "gpt-6.1-sol" {
		t.Fatal("invalid observation fixture", got.Reason)
	}
	return f
}

func TestCodexUnchangedSampleReadsOnlyMetaAndAnchor(t *testing.T) {
	f := unchangedCodexFixture(t)
	before := f.sample()
	budget := &readBudget{ctx: context.Background(), left: 1 << 20}
	state, changed, err := f.observer.observeCodex(budget, f.now)
	if err != nil || state != before.State || !changed.Equal(before.ChangedAt) {
		t.Fatal("unchanged observation changed", err)
	}
	read := 1<<20 - budget.left
	if read > 1024+anchorLimit {
		t.Fatalf("unchanged sample read %d bytes, want at most %d", read, 1024+anchorLimit)
	}
	after := f.sample()
	if after.State != before.State || after.Model != before.Model || after.Effort != before.Effort || after.Source != before.Source || after.Reason != before.Reason || !after.ChangedAt.Equal(before.ChangedAt) {
		t.Fatal("bounded metadata read changed observation")
	}
}

func TestSessionMetaLineBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, data, want string
		reason           error
	}{
		{"empty", "", "", malformed},
		{"short", "{}\nbody", "{}", nil},
		{"crlf", "{}\r\nbody", "{}\r", nil},
		{"expanded", strings.Repeat(" ", 8192) + "{}\nbody", strings.Repeat(" ", 8192) + "{}", nil},
		{"at-limit", strings.Repeat("x", metaLimit-1) + "\nbody", strings.Repeat("x", metaLimit-1), nil},
		{"short-incomplete", "{", "", malformed},
		{"limit-incomplete", strings.Repeat("x", metaLimit), "", budgetExceeded},
		{"newline-beyond-limit", strings.Repeat("x", metaLimit) + "\n", "", budgetExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &countingReaderAt{ReaderAt: strings.NewReader(tc.data)}
			budget := &readBudget{ctx: context.Background(), left: 1 << 20}
			got, err := budget.readSessionMeta(reader, int64(len(tc.data)))
			if err != tc.reason || string(got) != tc.want {
				t.Fatalf("metadata result length=%d reason=%v, want length=%d reason=%v", len(got), err, len(tc.want), tc.reason)
			}
			if reader.readBytes > metaLimit {
				t.Fatal("metadata read exceeded 64 KiB")
			}
		})
	}
}

func TestSessionMetaTruncationCancellationAndBudget(t *testing.T) {
	budget := &readBudget{ctx: context.Background(), left: 1 << 20}
	if _, err := budget.readSessionMeta(strings.NewReader("{}\n"), 4096); err != missing {
		t.Fatal("short read did not fail closed", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	budget = &readBudget{ctx: ctx, left: 1 << 20}
	if _, err := budget.readSessionMeta(strings.NewReader("{}\n"), 3); err != unavailable {
		t.Fatal("cancelled read accepted", err)
	}
	budget = &readBudget{ctx: context.Background(), left: 0}
	if _, err := budget.readSessionMeta(strings.NewReader("{}\n"), 3); err != budgetExceeded {
		t.Fatal("read budget ignored", err)
	}
}

func BenchmarkCodexUnchangedObservation(b *testing.B) {
	f := unchangedCodexFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if got := f.sample(); got.State != Idle {
			b.Fatal(got.Reason)
		}
	}
	b.StopTimer()
	budget := &readBudget{ctx: context.Background(), left: 1 << 20}
	if _, _, err := f.observer.observeCodex(budget, f.now); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(1<<20-budget.left), "read-B/op")
}
