package bugreport

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMisuseA6MarkupAndMultilingualReportFields(t *testing.T) {
	store, _ := fixtureStore(t)
	text := `<script>alert("fixture")</script><img src=x onerror=alert(1)> 한국어 日本語 العربية`
	in := Input{Title: text, Summary: text, Steps: text, Expected: text, Actual: text, Tool: "<script>x</script>", Severity: "high"}
	report, err := store.Create(in, reporter(1), "test")
	if err != nil {
		t.Fatal("valid multilingual markup report rejected")
	}
	got, err := store.Get(report.ID)
	if err != nil || got.Title != text || got.Summary != text || got.Steps != text || got.Expected != text || got.Actual != text || got.Tool != in.Tool {
		t.Fatal("report fields corrupted")
	}
	// Storage treats report content as data. JSON encoding escapes HTML; panel
	// rendering is separately owned and checked by the section-C worker.
	raw, err := json.Marshal(got)
	if err != nil || strings.Contains(string(raw), "<script>") {
		t.Fatal("JSON output contains literal HTML markup")
	}
	in.Severity = text
	if _, err := store.Create(in, reporter(1), "test"); err == nil {
		t.Fatal("markup severity accepted")
	}
	for i := 0; i < 9; i++ {
		if _, err := store.Create(fixtureInput(), reporter(1), "test"); err != nil {
			t.Fatal("quota rejected early")
		}
	}
	if _, err := store.Create(fixtureInput(), reporter(1), "test"); err == nil {
		t.Fatal("eleventh report accepted")
	}
}
