package i18n

import (
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func TestSubstitutionIsSinglePassAndCountsChooseTheForm(t *testing.T) {
	sessions := Entry{One: "{n} session", Other: "{n} sessions"}
	for n, want := range map[int]string{0: "0 sessions", 1: "1 session", 2: "2 sessions"} {
		if got := sessions.format([]any{"n", n}); got != want {
			t.Errorf("n=%d: %q, want %q", n, got, want)
		}
	}
	// A user-supplied value that looks like a placeholder or markup is copied, never interpreted again.
	remove := Entry{Other: "Remove {name} from {project}?"}
	if got := remove.format([]any{"name", "{project}", "project", "demo"}); got != "Remove {project} from demo?" {
		t.Errorf("argument re-scanned: %q", got)
	}
	if got := remove.format([]any{"name", "</script>{n}", "project", "p"}); got != "Remove </script>{n} from p?" {
		t.Errorf("argument changed: %q", got)
	}
	// Unknown or malformed placeholders stay visible so the render tests can catch them.
	if got := (Entry{Other: "a {missing} b {1x} {"}).format(nil); got != "a {missing} b {1x} {" {
		t.Errorf("malformed placeholder handling: %q", got)
	}
	if got := (Entry{Other: "언어를 저장했습니다."}).format([]any{"n", 1}); got != "언어를 저장했습니다." {
		t.Errorf("text without placeholders changed: %q", got)
	}
}

func TestFallbackGoesToEnglishThenToTheKey(t *testing.T) {
	saved := catalogs
	t.Cleanup(func() { catalogs = saved })
	catalogs = map[string]map[string]Entry{English: {"x.only_en": {Other: "English only"}}, Korean: {}}
	if got := T(Korean, "x.only_en"); got != "English only" {
		t.Errorf("Korean fallback = %q", got)
	}
	if got := T(Korean, "x.missing"); got != "x.missing" {
		t.Errorf("missing key = %q", got)
	}
	if got := T("fr", "x.only_en"); got != "English only" {
		t.Errorf("unsupported language = %q", got)
	}
}

func TestCatalogFilesRejectDuplicatesAndShapes(t *testing.T) {
	for name, files := range map[string]fstest.MapFS{
		"duplicate in a file":   {"en/a.json": {Data: []byte(`{"a.b":"x","a.b":"y"}`)}, "ko/a.json": {Data: []byte(`{}`)}},
		"duplicate across file": {"en/a.json": {Data: []byte(`{"a.b":"x"}`)}, "en/b.json": {Data: []byte(`{"a.b":"y"}`)}, "ko/a.json": {Data: []byte(`{}`)}},
		"number value":          {"en/a.json": {Data: []byte(`{"a.b":1}`)}, "ko/a.json": {Data: []byte(`{}`)}},
		"plural without other":  {"en/a.json": {Data: []byte(`{"a.b":{"one":"x"}}`)}, "ko/a.json": {Data: []byte(`{}`)}},
		"unknown plural form":   {"en/a.json": {Data: []byte(`{"a.b":{"few":"x","other":"y"}}`)}, "ko/a.json": {Data: []byte(`{}`)}},
		"trailing data":         {"en/a.json": {Data: []byte(`{"a.b":"x"} {}`)}, "ko/a.json": {Data: []byte(`{}`)}},
		"missing language":      {"en/a.json": {Data: []byte(`{"a.b":"x"}`)}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: accepted", name)
				}
			}()
			mustLoad(files)
		}()
	}
}

var hangul = regexp.MustCompile(`\p{Hangul}`)

// Every key exists in both languages with the same placeholders; English counts have both forms; values are plain
// text, English has no Hangul and nothing is empty.
func TestCatalogsAreCompleteAndConsistent(t *testing.T) {
	en, ko := Keys(English), Keys(Korean)
	if strings.Join(en, "\n") != strings.Join(ko, "\n") {
		t.Fatalf("key sets differ:\nonly en %v\nonly ko %v", diff(en, ko), diff(ko, en))
	}
	key := regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_-]+)+$`)
	for _, k := range en {
		e, _ := Lookup(English, k)
		o, _ := Lookup(Korean, k)
		if !key.MatchString(k) {
			t.Errorf("%s: key format", k)
		}
		for _, text := range []string{e.One, e.Other, o.One, o.Other} {
			if strings.ContainsAny(text, "<>") {
				t.Errorf("%s: markup in %q", k, text)
			}
		}
		// QA71 reviewed exception: native Korean language name; TestLanguageNamesStayNative fixes its exact value.
		if e.Other == "" || o.Other == "" || (hangul.MatchString(e.One+e.Other) && k != "profile.language_korean") {
			t.Errorf("%s: empty value or Hangul in English", k)
		}
		names := strings.Join(Placeholders(e.Other), ",")
		for _, text := range []string{e.One, o.One, o.Other} {
			if text != "" && strings.Join(Placeholders(text), ",") != names {
				t.Errorf("%s: placeholders differ: %q vs %q", k, text, e.Other)
			}
		}
		// {n} is reserved for counts, and an English count needs both forms (KO keeps one sentence).
		if contains(Placeholders(e.Other), "n") && e.One == "" {
			t.Errorf("%s: a counted English message needs one/other", k)
		}
	}
}

func contains(list []string, item string) bool {
	for _, v := range list {
		if v == item {
			return true
		}
	}
	return false
}

func diff(a, b []string) []string {
	in := map[string]bool{}
	for _, v := range b {
		in[v] = true
	}
	var out []string
	for _, v := range a {
		if !in[v] {
			out = append(out, v)
		}
	}
	return out
}

func TestJSMessagesCarryOnlyJSKeysAndCountForms(t *testing.T) {
	for _, language := range Languages {
		for key, value := range JS(language) {
			if !strings.HasPrefix(key, "js.") {
				t.Errorf("%s: %s is not a js key", language, key)
			}
			if forms, ok := value.(map[string]string); ok && forms["other"] == "" {
				t.Errorf("%s: %s count form without other", language, key)
			}
		}
	}
	if len(JS(English)) == 0 || len(JS(English)) != len(JS(Korean)) {
		t.Errorf("js catalogs: en %d, ko %d", len(JS(English)), len(JS(Korean)))
	}
}

// QA71: language names stay in their own language in both catalogs.
func TestLanguageNamesStayNative(t *testing.T) {
	for _, language := range Languages {
		if T(language, "profile.language_korean") != "한국어" {
			t.Errorf("%s: Korean language name changed", language)
		}
	}
}
