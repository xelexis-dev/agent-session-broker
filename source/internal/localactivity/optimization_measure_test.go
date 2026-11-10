package localactivity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Optional synthetic 2x2 audit; never scans an installed harness home.
func BenchmarkOptimizationObservation(b *testing.B) {
	if os.Getenv("ASB_OPT_BENCH") != "1" {
		b.Skip("opt-in synthetic optimization benchmark")
	}
	for _, broad := range []bool{false, true} {
		for _, valid := range []bool{false, true} {
			label := fmt.Sprintf("valid=%t/broad=%t", valid, broad)
			b.Run(label, func(b *testing.B) {
				f := newFixture(b, "codex")
				file, e := os.Create(f.path)
				if e != nil {
					b.Fatal("fixture")
				}
				file.WriteString(f.meta(sessionA))
				line := `{"type":"response_item","payload":{"type":"message","content":"` + strings.Repeat("x", 900) + `"}}` + "\n"
				for n := 0; n < 50<<20; n += len(line) {
					file.WriteString(line)
				}
				if valid {
					file.WriteString(f.event("task_started", "idle-turn") + f.event("task_complete", "idle-turn"))
				}
				file.Close()
				if broad {
					for i := 0; i < 414; i++ {
						d := filepath.Join(f.config.Home, "sessions/2026", fmt.Sprintf("%02d", 1+(i%37)/28), fmt.Sprintf("%02d", 1+(i%37)%28))
						os.MkdirAll(d, 0700)
						os.WriteFile(filepath.Join(d, fmt.Sprintf("rollout-fake-22222222-2222-4222-8222-%012d.jsonl", i)), []byte("{}\n"), 0600)
					}
				}
				first := &readBudget{ctx: context.Background(), left: 8 << 20}
				firstStart := time.Now()
				_, _, _ = f.observer.observeCodex(first, f.now)
				firstCost := time.Since(firstStart)
				firstRead := 8<<20 - first.left
				f.observer = New(f.config)
				got := f.sample()
				if valid && got.State != Idle {
					b.Fatal("invalid idle fixture", got.Reason)
				}
				if !valid && got.Reason != "evidence-missing" {
					b.Fatal("invalid missing fixture", got.Reason)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					got = f.sample()
					if valid && got.State != Idle {
						b.Fatal("idle changed")
					}
				}
				b.StopTimer()
				budget := &readBudget{ctx: context.Background(), left: 1 << 20}
				_, _, _ = f.observer.observeCodex(budget, f.now)
				b.ReportMetric(float64(1<<20-budget.left), "read-B/op")
				b.ReportMetric(float64(firstRead), "first-read-B")
				b.ReportMetric(float64(firstCost.Nanoseconds()), "first-ns")
			})
		}
	}
}
