package controlpanel

import (
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	texttemplate "text/template"
	"text/template/parse"

	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
)

var (
	jsKeyRE = regexp.MustCompile(`ASB\.t\(\s*["']([^"']+)["']`)
	goKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z0-9_.-]+$`)
)

// Every key the templates, Go handlers and panel scripts use exists in the catalog, and every catalog key is used
// (notice keys through their codes).
func TestCatalogKeysAreUsedAndExist(t *testing.T) {
	used := map[string]string{}
	files, err := productionPanelPaths(os.DirFS("."))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		switch {
		case strings.HasSuffix(name, ".html"):
			keys, err := panelTemplateKeyUses(name, data)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range keys {
				used[key] = name
			}
		case strings.HasSuffix(name, ".js"):
			for _, m := range jsKeyRE.FindAllStringSubmatch(string(withoutSourceComments(data)), -1) {
				used[m[1]] = name
			}
		case strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go"):
			keys, err := panelGoKeyUses(name, data)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range keys {
				used[key] = name
			}
		}
	}
	for _, key := range noticeMessages {
		used[key] = "noticeMessages"
	}
	for key, where := range used {
		if !i18n.Has(key) {
			t.Errorf("%s uses %q, which is not in the catalog", where, key)
		}
	}
	for _, key := range i18n.Keys(i18n.English) {
		if _, ok := used[key]; !ok {
			t.Errorf("catalog key %q is never used", key)
		}
	}
}

var sharedBlocks = []string{"page", "brand", "theme-toggle", "access", "toast", "dialog-close", "confirm-action"}

// sharedBlocksWithHangul returns every template reachable from the shared blocks (following {{template}} and
// {{block}} references recursively) whose parsed body contains Korean text in a
// text node or a string argument. src is templates.html; the other panel template files are read as production does.
func sharedBlocksWithHangul(src string) ([]string, error) {
	noop := func(...any) string { return "" }
	set := texttemplate.New("templates.html").Funcs(texttemplate.FuncMap{"t": noop, "tm": noop, "lang": noop,
		"jsMessages": noop, "asset": noop, "scopeCodes": noop})
	if _, err := set.Parse(src); err != nil {
		return nil, err
	}
	for _, name := range []string{"admin-api-panel.html", "direct-audit.html", "bug-reports.html", "token-scope-editor.html"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		if _, err := set.New(name).Parse(string(raw)); err != nil {
			return nil, err
		}
	}
	visited := map[string]bool{}
	var korean []string
	var visit func(string) error
	visit = func(name string) error {
		if visited[name] {
			return nil
		}
		visited[name] = true
		tmpl := set.Lookup(name)
		if tmpl == nil || tmpl.Tree == nil {
			return fmt.Errorf("template %q is referenced but not defined", name)
		}
		var refs []string
		if walkTemplate(tmpl.Tree.Root, &refs) {
			korean = append(korean, name)
		}
		for _, ref := range refs {
			if err := visit(ref); err != nil {
				return err
			}
		}
		return nil
	}
	for _, name := range sharedBlocks {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	sort.Strings(korean)
	return korean, nil
}

// walkTemplate reports Korean in every text node and string argument of a tree and collects the templates it calls.
func walkTemplate(node parse.Node, refs *[]string, keySets ...map[string]bool) bool {
	found := false
	visit := func(n parse.Node) { found = walkTemplate(n, refs, keySets...) || found }
	switch n := node.(type) {
	case *parse.ListNode:
		if n != nil {
			for _, child := range n.Nodes {
				visit(child)
			}
		}
	case *parse.TextNode:
		found = hangulRE.Match(n.Text)
	case *parse.StringNode:
		found = hangulRE.MatchString(n.Text)
	case *parse.ActionNode:
		visit(n.Pipe)
	case *parse.PipeNode:
		if n != nil {
			for _, cmd := range n.Cmds {
				visit(cmd)
			}
		}
	case *parse.CommandNode:
		if len(keySets) > 1 && len(n.Args) > 1 {
			if fun, ok := n.Args[0].(*parse.IdentifierNode); ok && (fun.Ident == "eq" || fun.Ident == "ne") {
				view := false
				for _, arg := range n.Args[1:] {
					switch value := arg.(type) {
					case *parse.FieldNode:
						view = view || (len(value.Ident) == 1 && value.Ident[0] == "View")
					case *parse.VariableNode:
						view = view || (len(value.Ident) == 2 && value.Ident[0] == "$" && value.Ident[1] == "View")
					}
				}
				if view {
					for _, arg := range n.Args[1:] {
						if value, ok := arg.(*parse.StringNode); ok {
							keySets[1][value.Text] = true
						}
					}
				}
			}
		}
		if len(keySets) > 0 && len(n.Args) > 1 {
			if fun, ok := n.Args[0].(*parse.IdentifierNode); ok && fun.Ident == "t" {
				if key, ok := n.Args[1].(*parse.StringNode); ok {
					keySets[0][key.Text] = true
				}
			}
		}
		for _, arg := range n.Args {
			visit(arg)
		}
	case *parse.IfNode:
		visit(n.Pipe)
		visit(n.List)
		visit(n.ElseList)
	case *parse.RangeNode:
		visit(n.Pipe)
		visit(n.List)
		visit(n.ElseList)
	case *parse.WithNode:
		visit(n.Pipe)
		visit(n.List)
		visit(n.ElseList)
	case *parse.TemplateNode:
		*refs = append(*refs, n.Name)
		visit(n.Pipe)
	}
	return found
}

// astra R2-G1: a block's check must cover its whole body, including text after nested {{end}} lines.
func TestSharedBlockCheckCoversWholeBlocks(t *testing.T) {
	raw, err := os.ReadFile("templates.html")
	if err != nil {
		t.Fatal(err)
	}
	marker := `{{template "confirm-action" .}}</main>`
	if strings.Count(string(raw), marker) != 1 {
		t.Fatal("page block marker not found")
	}
	for _, at := range []string{marker, `<a class="skip-link"`} {
		mutated := strings.Replace(string(raw), at, "<span>번역 누락</span>"+at, 1)
		if korean, err := sharedBlocksWithHangul(mutated); err != nil || strings.Join(korean, ",") != "page" {
			t.Errorf("Korean before %q not reported for the page block: %v (%v)", at, korean, err)
		}
	}
	// astra R2: text in a template the shared block calls ({{block}} or {{template}} of a new define) counts too.
	nested := map[string]string{
		"block":    strings.Replace(string(raw), marker, `{{block "extra-shared" .}}<span>번역 누락</span>{{end}}`+marker, 1),
		"template": strings.Replace(string(raw), marker, `{{template "extra-defined" .}}`+marker, 1) + `{{define "extra-defined"}}{{template "extra-deeper"}}{{end}}{{define "extra-deeper"}}<span>번역 누락</span>{{end}}`,
	}
	for kind, mutated := range nested {
		if korean, err := sharedBlocksWithHangul(mutated); err != nil || len(korean) == 0 {
			t.Errorf("Korean in a %s reachable from the page block not reported: %v (%v)", kind, korean, err)
		}
	}
	// A Korean string argument inside nested actions counts too.
	mutated := strings.Replace(string(raw), `{{t "heading.add_project"}}`, `{{if eq .View "누락"}}{{end}}{{t "heading.add_project"}}`, 1)
	if korean, err := sharedBlocksWithHangul(mutated); err != nil || strings.Join(korean, ",") != "page" {
		t.Errorf("Korean string argument not reported: %v (%v)", korean, err)
	}
}

// No panel production source is deferred. The only exemptions are test fixtures, whose purpose includes Unicode data.
func TestNoHangulOutsideTheKoreanCatalog(t *testing.T) {
	findings, err := panelSourceFindings(os.DirFS("."))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Errorf("panel production sources still contain Hangul: %v", findings)
	}
	raw, err := fs.ReadFile(os.DirFS("."), "templates.html")
	if err != nil {
		t.Fatal(err)
	}
	if korean, err := sharedBlocksWithHangul(string(raw)); err != nil || len(korean) > 0 {
		t.Errorf("template blocks still contain Hangul: %v (%v)", korean, err)
	}
}

// panel.js defines window.ASB; classic deferred and non-async module scripts run in document order, so it must come
// first, deferred and not async, before every script that calls ASB.t.
func TestPanelScriptDefinesASBBeforeItsUsers(t *testing.T) {
	raw, err := os.ReadFile("templates.html")
	if err != nil {
		t.Fatal(err)
	}
	head := string(raw)[:strings.Index(string(raw), "</head>")]
	tags := regexp.MustCompile(`<script [^>]*src="\{\{asset "/assets/([a-z-]+\.js)"\}\}"[^>]*>`).FindAllStringSubmatch(head, -1)
	position := map[string]int{}
	for i, m := range tags {
		position[m[1]] = i
		if strings.Contains(m[0], " async") {
			t.Errorf("%s is async; its order is not guaranteed", m[1])
		}
	}
	panel, ok := position["panel.js"]
	if !ok || !strings.Contains(tags[panel][0], " defer") {
		t.Fatalf("panel.js missing or not deferred: %v", tags)
	}
	for name, at := range position {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "panel.js" || !strings.Contains(string(data), "ASB.t(") {
			continue
		}
		if at < panel || !(strings.Contains(tags[at][0], " defer") || strings.Contains(tags[at][0], `type="module"`)) {
			t.Errorf("%s uses ASB but can run before panel.js", name)
		}
	}
	if _, ok := position["select.js"]; !ok {
		t.Error("select.js tag not found")
	}
}

func TestSharedPanelMessagesAreCatalogKeys(t *testing.T) {
	for index, key := range []string{settingsConflictMessage, tokenScopeUpgradeMessage, tokenScopeInvalidMessage, tokenCapMessage, projectDataUnavailableNotice} {
		if !i18n.Has(key) || hangulRE.MatchString(key) {
			t.Errorf("shared panel message %d is not a catalog key", index)
		}
	}
}
