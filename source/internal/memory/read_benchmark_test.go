package memory

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readBenchmarkStore(tb testing.TB, n, bodyBytes int) (*Store, string) {
	tb.Helper()
	store, e := NewStore(tb.TempDir())
	if e != nil {
		tb.Fatal(e)
	}
	tb.Cleanup(func() { store.Close() })
	first := ""
	for i := 0; i < n; i++ {
		node := Node{ID: fmt.Sprintf("mem-%032x", i+1), Title: "benchmark", Body: strings.Repeat("x", bodyBytes), CreatedBy: "synthetic", CreatedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)}
		if i == 0 {
			first = node.ID
		}
		var raw bytes.Buffer
		if encodeNode(&raw, node) != nil {
			tb.Fatal("encode fixture")
		}
		if os.WriteFile(filepath.Join(store.Root(), "nodes", node.ID+".md"), raw.Bytes(), 0600) != nil {
			tb.Fatal("write fixture")
		}
	}
	return store, first
}
func benchmarkStoreGet(b *testing.B, store *Store, id string) {
	b.Helper()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, e := store.Get(ctx, id); e != nil {
			b.Fatal(e)
		}
	}
}
func BenchmarkMemoryGetScan(b *testing.B) {
	for _, n := range []int{1, 100, 1000} {
		b.Run(fmt.Sprint("nodes", n, "-4KiB"), func(b *testing.B) { store, id := readBenchmarkStore(b, n, 4096); benchmarkStoreGet(b, store, id) })
	}
	b.Run("nodes1-maxBody", func(b *testing.B) {
		store, id := readBenchmarkStore(b, 1, MaxBodyBytes)
		benchmarkStoreGet(b, store, id)
	})
}
func TestSmallMemoryGetAllocationDiagnostic(t *testing.T) {
	store, id := readBenchmarkStore(t, 1, 4096)
	result := testing.Benchmark(func(b *testing.B) { benchmarkStoreGet(b, store, id) })
	t.Logf("small read: %d B/op %d allocs/op", result.AllocedBytesPerOp(), result.AllocsPerOp())
}

func TestSmallMemoryReadAllocation(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 4096)
	ctx := context.Background()
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			got, err := readPersistedNode(ctx, bytes.NewReader(payload), int64(len(payload)))
			if err != nil || !bytes.Equal(got, payload) {
				b.Fatalf("small persisted read: len=%d err=%v", len(got), err)
			}
		}
	})
	t.Logf("small reader: %d B/op %d allocs/op", result.AllocedBytesPerOp(), result.AllocsPerOp())
	// Isolate the buffer guard from filesystem validation and YAML allocations.
	if result.AllocedBytesPerOp() > 8<<10 {
		t.Fatal("small persisted read allocates oversized buffers")
	}
}
