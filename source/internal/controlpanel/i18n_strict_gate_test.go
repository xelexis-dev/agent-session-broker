package controlpanel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"html"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	texttemplate "text/template"
	"unicode"
	"unicode/utf8"
)

// Embed directives override fixture naming: a served/embedded fixture is production input.
func productionPanelPaths(root fs.FS) ([]string, error) {
	var files []string
	embedded := map[string]bool{}
	err := fs.WalkDir(root, ".", func(file string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, file)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	embedded, err = panelEmbedFiles(root, files)
	if err != nil {
		return nil, err
	}

	var paths []string
	for _, name := range files {
		fixture := strings.HasSuffix(name, "_test.go") || strings.Contains("/"+name, "/testdata/")
		if !embedded[name] && fixture {
			continue
		}
		textSource := strings.HasSuffix(name, ".go") || strings.HasSuffix(name, ".html") || strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".css")
		if !embedded[name] && !textSource {
			continue
		}
		data, err := fs.ReadFile(root, name)
		if err != nil {
			return nil, err
		}
		// Embedded fonts are binary. Every UTF-8 text asset (including SVG/JSON) participates.
		if textSource || strings.HasSuffix(name, ".svg") || strings.HasSuffix(name, ".json") || (utf8.Valid(data) && !strings.ContainsRune(string(data), 0)) {
			paths = append(paths, name)
		}
	}
	return paths, nil
}

// Go resolves quoting, directory/glob rules and all:; fixture files use the same offline toolchain.
func panelEmbedFiles(root fs.FS, files []string) (map[string]bool, error) {
	dir, err := os.MkdirTemp("", "asb-embed-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	hasGo := false
	for _, name := range files {
		data, err := fs.ReadFile(root, name)
		if err != nil {
			return nil, err
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, data, 0600); err != nil {
			return nil, err
		}
		hasGo = hasGo || (!strings.Contains(name, "/") && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go"))
	}
	if !hasGo {
		if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte("package panel\n"), 0600); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module panel-gate-fixture\n"), 0600); err != nil {
		return nil, err
	}
	executable, err := panelGoExecutable(runtime.GOROOT())
	if err != nil {
		return nil, fmt.Errorf("find Go embed resolver: %w", err)
	}
	cmd := exec.Command(executable, "list", "-json", "-find", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GO111MODULE=on")
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("resolve panel embed files: %w", err)
	}
	embedded := map[string]bool{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	packages := 0
	for {
		var pkg struct {
			Dir        string
			EmbedFiles []string
			Error      *struct{ Err string }
			Incomplete bool
		}
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if pkg.Error != nil || pkg.Incomplete {
			return nil, fmt.Errorf("incomplete panel embed set")
		}
		packages++
		for _, name := range pkg.EmbedFiles {
			relative, err := filepath.Rel(dir, filepath.Join(pkg.Dir, filepath.FromSlash(name)))
			if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("panel embed outside scan root")
			}
			embedded[filepath.ToSlash(relative)] = true
		}
	}
	if packages == 0 {
		return nil, fmt.Errorf("no packages in panel embed scan")
	}
	return embedded, nil
}

func panelGoExecutable(goRoot string) (string, error) {
	if goRoot != "" {
		if executable, err := exec.LookPath(filepath.Join(goRoot, "bin", "go")); err == nil {
			return executable, nil
		}
	}
	return exec.LookPath("go")
}

func panelSourceFindings(root fs.FS) ([]string, error) {
	paths, err := productionPanelPaths(root)
	if err != nil {
		return nil, err
	}
	var findings []string
	for _, path := range paths {
		data, err := fs.ReadFile(root, path)
		if err != nil {
			return nil, err
		}
		if panelSourceHasHangul(path, data) {
			findings = append(findings, path)
		}
	}
	sort.Strings(findings)
	return findings, nil
}

var jsStringConstruction = regexp.MustCompile(`\bString\s*\.\s*(?:fromCodePoint|fromCharCode)\s*\(`)

// Construction checks inspect code tokens, never string/comment/template contents.
func panelConstructionHasHangul(name string, data []byte) bool {
	javascript := strings.HasSuffix(name, ".js")
	if javascript {
		data = jsConstructionCode(data)
		if jsStringConstruction.Match(data) {
			return true
		}
	}
	var scan scanner.Scanner
	set := token.NewFileSet()
	scan.Init(set.AddFile("source", -1, len(data)), data, nil, 0)
	for {
		_, kind, lit := scan.Scan()
		if kind == token.EOF {
			return false
		}
		if kind == token.INT {
			if value, err := strconv.ParseUint(strings.ReplaceAll(lit, "_", ""), 0, 32); err == nil && unicode.Is(unicode.Hangul, rune(value)) {
				return true
			}
		}
		if !javascript && (kind == token.STRING || kind == token.CHAR) {
			if value, err := strconv.Unquote(lit); err == nil && hangulRE.MatchString(value) {
				return true
			}
			if lit[0] == '`' {
				continue
			}
			for rest := lit[1 : len(lit)-1]; rest != ""; {
				value, multibyte, tail, err := strconv.UnquoteChar(rest, lit[0])
				if err != nil {
					break
				}
				if !multibyte && value >= 128 {
					return true
				}
				rest = tail
			}
		}
	}
}

func jsConstructionCode(data []byte) []byte {
	code := append([]byte(nil), data...)
	// A template expression has its own brace depth; quoted text and nested templates push separate frames.
	type frame struct {
		kind  byte
		depth int
	}
	stack := []frame{{kind: 'c'}}
	mask := func(i int) {
		if i < len(code) && code[i] != '\n' {
			code[i] = ' '
		}
	}
	for i := 0; i < len(data); {
		top := &stack[len(stack)-1]
		ch := data[i]
		if top.kind == '\'' || top.kind == '"' {
			mask(i)
			i++
			if ch == '\\' {
				mask(i)
				if i < len(data) {
					i++
				}
			} else if ch == top.kind {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		if top.kind == '`' {
			mask(i)
			i++
			switch {
			case ch == '\\':
				mask(i)
				if i < len(data) {
					i++
				}
			case ch == '`':
				stack = stack[:len(stack)-1]
			case ch == '$' && i < len(data) && data[i] == '{':
				mask(i)
				i++
				stack = append(stack, frame{kind: '$', depth: 1})
			}
			continue
		}
		switch {
		case ch == '\'' || ch == '"' || ch == '`':
			mask(i)
			i++
			stack = append(stack, frame{kind: ch})
		case ch == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				mask(i)
				i++
			}
		case ch == '/' && i+1 < len(data) && data[i+1] == '*':
			mask(i)
			mask(i + 1)
			i += 2
			for i < len(data) {
				end := data[i] == '*' && i+1 < len(data) && data[i+1] == '/'
				mask(i)
				i++
				if end {
					mask(i)
					i++
					break
				}
			}
		default:
			if top.kind == '$' {
				if ch == '{' {
					top.depth++
				}
				if ch == '}' {
					top.depth--
					if top.depth == 0 {
						mask(i)
						stack = stack[:len(stack)-1]
					}
				}
			}
			i++
		}
	}
	return code
}

// Consume each escape once: an escaped backslash cannot introduce another Unicode escape.
func panelDecodedEscapes(source string, css bool) string {
	var out strings.Builder
	for i := 0; i < len(source); {
		if source[i] != '\\' || i+1 == len(source) {
			out.WriteByte(source[i])
			i++
			continue
		}
		i++
		start := i
		if css {
			for i < len(source) && i-start < 6 && strings.ContainsRune("0123456789abcdefABCDEF", rune(source[i])) {
				i++
			}
			if i > start {
				value, _ := strconv.ParseUint(source[start:i], 16, 32)
				out.WriteRune(rune(value))
				if i < len(source) && strings.ContainsRune(" \t\n\r\f", rune(source[i])) {
					if source[i] == '\r' && i+1 < len(source) && source[i+1] == '\n' {
						i++
					}
					i++
				}
				continue
			}
		} else {
			size := 0
			byteEscape := source[i] == 'x'
			if byteEscape {
				size = 2
			} else if source[i] == 'u' {
				size = 4
			}
			if size > 0 {
				i++
				start = i
				if !byteEscape && i < len(source) && source[i] == '{' {
					i++
					start = i
					for i < len(source) && source[i] != '}' {
						i++
					}
					if i < len(source) {
						if value, err := strconv.ParseUint(source[start:i], 16, 32); err == nil {
							out.WriteRune(rune(value))
							i++
							continue
						}
					}
				} else if i+size <= len(source) {
					if value, err := strconv.ParseUint(source[i:i+size], 16, 32); err == nil {
						if byteEscape {
							out.WriteByte(byte(value))
						} else {
							out.WriteRune(rune(value))
						}
						i += size
						continue
					}
				}
				i = start
			}
		}
		if i < len(source) {
			out.WriteByte(source[i])
			i++
		}
	}
	return out.String()
}

func panelSourceHasHangul(name string, data []byte) bool {
	if hangulRE.Match(data) {
		return true
	}
	if strings.HasSuffix(name, ".go") {
		return panelConstructionHasHangul(name, data)
	}
	if strings.HasSuffix(name, ".js") && panelConstructionHasHangul(name, data) {
		return true
	}
	return hangulRE.MatchString(html.UnescapeString(panelDecodedEscapes(string(data), strings.HasSuffix(name, ".css"))))
}

func goLiteralStrings(data []byte) []string {
	var scan scanner.Scanner
	set := token.NewFileSet()
	scan.Init(set.AddFile("source", -1, len(data)), data, nil, 0)
	var values []string
	for {
		_, kind, lit := scan.Scan()
		if kind == token.EOF {
			return values
		}
		if kind == token.STRING || kind == token.CHAR {
			if value, err := strconv.Unquote(lit); err == nil {
				values = append(values, value)
			}
		}
	}
}

func TestStrictPanelSourceGateRejectsHiddenHangul(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"nested/view.html", "<p>번역 누락</p>"},
		{"handler.go", `package panel; const label="\uD55C\uAD6D\uC5B4"`},
		{"panel.js", `const label="\uD55C\uAD6D\uC5B4";`},
		{"panel.js", `const label="\u{D55C}";`},
		{"panel.js", `const label="\xED\x95\x9C";`},
		{"view.html", `<p title="&#54620;">text</p>`},
		{"panel.js", "// ᄀ is Hangul jamo too\n"},
		{"handler.go", "package panel // 한글\n"},
	} {
		t.Run(tc.name+tc.source[:1], func(t *testing.T) {
			found, err := panelSourceFindings(fstest.MapFS{tc.name: &fstest.MapFile{Data: []byte(tc.source)}})
			if err != nil || len(found) != 1 || found[0] != tc.name {
				t.Errorf("untranslated source accepted: %v (%v)", found, err)
			}
		})
	}
	found, err := panelSourceFindings(fstest.MapFS{"panel.js": &fstest.MapFile{Data: []byte(`const label="English";`)}, "fixture_test.go": &fstest.MapFile{Data: []byte("package panel // 한글 fixture\n")}, "testdata/fixture.js": &fstest.MapFile{Data: []byte("package panel // 한글 fixture\n")}})
	if err != nil || len(found) != 0 {
		t.Errorf("English source/test fixtures rejected: %v (%v)", found, err)
	}
}

// QA71 permits exactly one native Korean option in the account selector, only on /profile.
var nativeKoreanOption = regexp.MustCompile(`<option value="ko" lang="ko"(?: selected)?>한국어</option>`)
var accountLanguageSelect = regexp.MustCompile(`<select id="profile-language" name="language" data-select>(?:[^<]|<option\b[^>]*>[^<]*</option>)*</select>`)

func englishPageHasHangul(path, body string) bool {
	if path != "/profile" {
		return hangulRE.MatchString(html.UnescapeString(body))
	}
	selectors := accountLanguageSelect.FindAllStringIndex(body, -1)
	if len(selectors) != 1 || strings.Count(body, `id="profile-language"`) != 1 || len(nativeKoreanOption.FindAllStringIndex(body, -1)) != 1 {
		return true
	}
	at := selectors[0]
	selected := body[at[0]:at[1]]
	if !nativeKoreanOption.MatchString(selected) {
		return true
	}
	body = body[:at[0]] + nativeKoreanOption.ReplaceAllString(selected, "") + body[at[1]:]
	return hangulRE.MatchString(html.UnescapeString(body))
}

func TestEnglishPageHangulExceptionIsExact(t *testing.T) {
	option := `<option value="ko" lang="ko">한국어</option>`
	allowed := `<select id="profile-language" name="language" data-select>` + option + `</select>`
	if englishPageHasHangul("/profile", allowed) {
		t.Fatal("QA71 native language option rejected")
	}
	for _, body := range []string{allowed + "한국어", `<span>한국어</span>`, `<span>&#54620;</span>`, `<option value="en" lang="ko">한국어</option>`, "ᄀ", allowed + allowed, strings.Replace(allowed, option, option+option, 1)} {
		if !englishPageHasHangul("/profile", body) {
			t.Error("Hangul outside the unique account language selector accepted")
		}
	}
	if !englishPageHasHangul("/", allowed) {
		t.Error("profile exception accepted on another page")
	}
	if englishPageHasHangul("/", "<p>English</p>") {
		t.Error("English page rejected")
	}
}

// These four dotted strings are filenames, not messages. Exceptions are tied to the reviewed source file and value.
var panelNonMessageLiterals = map[[2]string]string{
	{"sqlite.go", "control.db"}:          "SQLite control database filename",
	{"sqlite.go", "control.json"}:        "legacy control import filename",
	{"store.go", "control.lock"}:         "control-store process lock filename",
	{"i18n_render.go", "templates.html"}: "template entry filename",
}

// Every key-shaped Go literal participates, including a misspelled namespace. Translation calls override filename exemptions.

func panelGoKeyUses(name string, data []byte) ([]string, error) {
	var keys []string
	seen := map[string]bool{}
	add := func(key string) {
		if !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	for _, value := range goLiteralStrings(data) {
		if goKeyRE.MatchString(value) && panelNonMessageLiterals[[2]string{name, value}] == "" {
			add(value)
		}
	}
	tree, err := parser.ParseFile(token.NewFileSet(), name, data, 0)
	if err != nil {
		return nil, err
	}
	ast.Inspect(tree, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		keyIndex := -1
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if owner, ok := fun.X.(*ast.Ident); ok && owner.Name == "i18n" {
				switch fun.Sel.Name {
				case "T", "Lookup":
					keyIndex = 1
				case "M", "Has":
					keyIndex = 0
				}
			}
		case *ast.Ident:
			if fun.Name == "localText" {
				keyIndex = 1
			}
		}
		if keyIndex >= 0 && len(call.Args) > keyIndex {
			if lit, ok := call.Args[keyIndex].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if key, err := strconv.Unquote(lit.Value); err == nil {
					add(key)
				}
			}
		}
		return true
	})
	return keys, nil
}

func gateTemplates(name string, data []byte) (*texttemplate.Template, error) {
	noop := func(...any) string { return "" }
	return texttemplate.New(name).Funcs(texttemplate.FuncMap{"t": noop, "tm": noop, "lang": noop, "jsMessages": noop, "asset": noop, "scopeCodes": noop}).Parse(string(data))
}
func panelTemplateKeyUses(name string, data []byte) ([]string, error) {
	set, err := gateTemplates(name, data)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, tpl := range set.Templates() {
		var refs []string
		walkTemplate(tpl.Tree.Root, &refs, keys)
	}
	var result []string
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result, nil
}

func withoutSourceComments(data []byte) []byte {
	copy := append([]byte(nil), data...)
	var scan scanner.Scanner
	set := token.NewFileSet()
	file := set.AddFile("script", -1, len(data))
	scan.Init(file, data, nil, scanner.ScanComments)
	for {
		position, kind, lit := scan.Scan()
		if kind == token.EOF {
			return copy
		}
		if kind == token.COMMENT {
			offset := file.Offset(position)
			for at := offset; at < offset+len(lit); at++ {
				if copy[at] != '\n' {
					copy[at] = ' '
				}
			}
		}
	}
}

func TestPanelKeyCollectorsCoverNamespacesAndQuotedTemplates(t *testing.T) {
	keys, err := panelGoKeyUses("probe.go", []byte(`package panel;const label="new_surface.label";func probe(){i18n.T("en","new_surface.missing");localText("en","new_surface.error")}; // i18n.T("en","comment.only")`))
	if err != nil || strings.Join(keys, ",") != "new_surface.label,new_surface.missing,new_surface.error" {
		t.Errorf("explicit Go keys lost or comments counted: %v (%v)", keys, err)
	}
	keys, err = panelTemplateKeyUses("probe.html", []byte("{{define \"probe\"}}{{ t `new_surface.missing` }}{{/* t \"comment.only\" */}}{{end}}"))
	if err != nil || strings.Join(keys, ",") != "new_surface.missing" {
		t.Errorf("quoted template key lost: %v (%v)", keys, err)
	}
	for _, tc := range []struct {
		name, source string
		want         int
	}{
		{"sqlite.go", `package p;const file="control.db"`, 0},
		{"other.go", `package p;const label="control.db"`, 1},
		{"sqlite.go", `package p;func label(){i18n.T("en","control.db")}`, 1},
	} {
		values, err := panelGoKeyUses(tc.name, []byte(tc.source))
		if err != nil || len(values) != tc.want {
			t.Errorf("metadata exception escaped its source/use boundary: %v (%v)", values, err)
		}
	}
	for _, reason := range panelNonMessageLiterals {
		if reason == "" {
			t.Error("unreviewed filename exception")
		}
	}
	script := withoutSourceComments([]byte("// ASB.t('comment.only')\nconst url='http://example.test';ASB.t('new_surface.missing');/*ASB.t('comment.two')*/"))
	hits := jsKeyRE.FindAllStringSubmatch(string(script), -1)
	if len(hits) != 1 || hits[0][1] != "new_surface.missing" {
		t.Errorf("JS key lost or comments counted: %d", len(hits))
	}
}

// Keep the source canary helper's error path visible: missing files never turn a scan into success.
func TestStrictPanelSourceGateFailsOnReadErrors(t *testing.T) {
	_, err := panelSourceFindings(brokenPanelFS{})
	if err == nil {
		t.Fatal("unreadable source accepted")
	}
}

type brokenPanelFS struct{}

func (brokenPanelFS) Open(string) (fs.File, error) { return nil, fs.ErrPermission }

func TestStrictPanelReviewAssetInputs(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"panel.css", `main::before { content: "한글"; }`},
		{"panel.css", `main::before { content: "\d55c\ae00"; }`},
		{"testdata/banner.js", `document.body.append("한글")`},
		{"testdata/banner_test.go", `한글`},
		{"testdata/banner.svg", `<svg><text>한글</text></svg>`},
	} {
		root := fstest.MapFS{"embed.go": &fstest.MapFile{Data: []byte("package panel\nimport _ \"embed\"\n//go:embed " + tc.name + "\nvar input string\n")}, tc.name: &fstest.MapFile{Data: []byte(tc.source)}}
		findings, err := panelSourceFindings(root)
		if err != nil || len(findings) != 1 || findings[0] != tc.name {
			t.Errorf("production asset accepted: %s %v (%v)", tc.name, findings, err)
		}
	}
}
func TestStrictPanelReviewConstruction(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"handler.go", `package p;const s="\xed"+"\x95"+"\x9c"`},
		{"handler.go", `package p;const s="\355"+"\225"+"\234"`},
		{"handler.go", `package p;var s=string([]rune{54620,44544})`},
		{"handler.go", `package p;var s=0xD55C`},
		{"handler.go", `package p;var s='\u3131'`},
		{"panel.js", `document.body.append(String.fromCodePoint(54620,44544))`},
		{"panel.js", `document.body.append(String.fromCharCode(65))`},
		{"panel.js", `const s=0xD55C;`},
		{"panel.js", `const s=12593;`},
		{"panel.js", `const s=4352;`},
		{"panel.js", `const s=0xA960;`},
		{"panel.js", `const s=0xD7B0;`},
		{"handler.go", `package p;var s=0x1100`},
	} {
		if !panelSourceHasHangul(tc.name, []byte(tc.source)) {
			t.Errorf("construction accepted: %s %s", tc.name, tc.source)
		}
	}
}
func TestStrictPanelReviewNativeLocation(t *testing.T) {
	option := `<option value="ko" lang="ko">한국어</option>`
	for _, body := range []string{
		`<select id="unrelated">` + option + `</select>`, option + option,
		`<select id="profile-language" name="language" data-select>` + option + option + `</select>`,
	} {
		if !englishPageHasHangul("/profile", body) {
			t.Errorf("native option accepted outside its unique account selector")
		}
	}
}

func TestStrictPanelRound2EmbedRules(t *testing.T) {
	for _, tc := range []struct{ directive, file string }{
		{`//go:embed "testdata/banner note.txt"`, "testdata/banner note.txt"},
		{"//go:embed `testdata/banner note.txt`", "testdata/banner note.txt"},
		{"  //go:embed testdata/banner.txt", "testdata/banner.txt"},
		{"//go:embed testdata/banner.txt testdata/other.txt", "testdata/banner.txt"},
		{"//go:embed all:testdata", "testdata/_banner.txt"},
		{"//go:embed testdata", "testdata/nested/banner.txt"},
		{"//go:embed testdata/*.txt", "testdata/banner.txt"},
	} {
		t.Run(tc.directive, func(t *testing.T) {
			root := fstest.MapFS{"embed.go": &fstest.MapFile{Data: []byte("package panel\nimport _ \"embed\"\n" + tc.directive + "\nvar input string\n")}, tc.file: &fstest.MapFile{Data: []byte("한글")}, "testdata/other.txt": &fstest.MapFile{Data: []byte("English")}}
			findings, err := panelSourceFindings(root)
			if err != nil || len(findings) != 1 || findings[0] != tc.file {
				t.Errorf("embed variant accepted: %v (%v)", findings, err)
			}
		})
	}
	for _, source := range []string{
		"package panel\nimport _ \"embed\"\n//go:embed missing.txt\nvar input string\n",
		"invalid Go source",
	} {
		if _, err := panelSourceFindings(fstest.MapFS{"embed.go": &fstest.MapFile{Data: []byte(source)}}); err == nil {
			t.Error("unresolvable embed set accepted")
		}
	}

}
func TestStrictPanelRound2LiteralBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"panel.js", `const help="Use String.fromCharCode for ASCII.";`},
		{"handler.go", `package panel;const help="Write \\xED for a byte escape."`},
		{"panel.css", `main::before { content:"\\d55c"; }`},
		{"panel.js", "const help=`String.fromCodePoint(54620)`;"},
		{"panel.js", `const help='54620 String.fromCodePoint(65)';`},
		{"panel.js", `/* String.fromCodePoint(54620) */ const limit=80;`},
		{"panel.js", `const help="Write \\uD55C literally";`},
		{"handler.go", "package panel;const help=`\\xED`"},
		{"handler.go", `package panel;const help="54620 \\355"`},
	} {
		if panelSourceHasHangul(tc.name, []byte(tc.source)) {
			t.Errorf("English literal rejected: %s", tc.source)
		}
	}
}

func TestStrictPanelRound3TemplateExpressions(t *testing.T) {
	for _, source := range []string{
		"document.body.append(`${String.fromCodePoint(54620,44544)}`)",
		"const text=`${54620}`;",
		"const text=`${{nested:`${String.fromCharCode(54620)}`}.nested}`;",
		"const text=`${{outer:{inner:`${{value:54620}.value}`}}.outer.inner}`;",
		"const text=`${\"plain `template ${54620}`\"}${String.fromCodePoint(65)}`;",
	} {
		if !panelSourceHasHangul("panel.js", []byte(source)) {
			t.Errorf("template expression accepted: %s", source)
		}
	}
	for _, source := range []string{
		"const text=`plain String.fromCodePoint(54620)`;",
		"const text=`${\"plain `template ${String.fromCodePoint(54620)}`\"}`;",
		"const text=`${{nested:`plain ${\"String.fromCodePoint(54620)\"}`}.nested}`;",
	} {
		if panelSourceHasHangul("panel.js", []byte(source)) {
			t.Errorf("plain template/string text rejected: %s", source)
		}
	}
}
func TestStrictPanelRound3NestedEmbeds(t *testing.T) {
	root := fstest.MapFS{"root.go": &fstest.MapFile{Data: []byte("package panel\n")}}
	for _, name := range []string{"widget", "other/nested"} {
		root[name+"/embed.go"] = &fstest.MapFile{Data: []byte("package widget\nimport _ \"embed\"\n//go:embed testdata/banner.txt\nvar input string\n")}
		root[name+"/testdata/banner.txt"] = &fstest.MapFile{Data: []byte("한글")}
	}
	findings, err := panelSourceFindings(root)
	if err != nil || strings.Join(findings, ",") != "other/nested/testdata/banner.txt,widget/testdata/banner.txt" {
		t.Errorf("nested package embed set incomplete: %v (%v)", findings, err)
	}
}

func TestStrictPanelRound3GoExecutable(t *testing.T) {
	executable, err := panelGoExecutable(runtime.GOROOT())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(executable))
	if _, err := panelGoExecutable(""); err != nil {
		t.Fatalf("empty GOROOT must use PATH: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := panelGoExecutable(""); err == nil {
		t.Fatal("missing GOROOT and PATH Go resolver accepted")
	}
}
