package serverruntime

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type outputSink struct{ File, Function, Kind, Expression string }

func sinkCalls(name string, src []byte) ([]outputSink, error) {
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, name, src, 0)
	if err != nil {
		return nil, err
	}
	imports := map[string]string{}
	for _, v := range f.Imports {
		path, _ := strconv.Unquote(v.Path.Value)
		alias := filepath.Base(path)
		if v.Name != nil {
			alias = v.Name.Name
		}
		imports[alias] = path
	}
	var out []outputSink
	for _, v := range f.Imports {
		if v.Name != nil && v.Name.Name == "." {
			out = append(out, outputSink{name, "<package>", "dot-import", v.Path.Value})
		}
	}
	normalized := func(n ast.Node) string { var b bytes.Buffer; _ = format.Node(&b, fs, n); return b.String() }
	for _, decl := range f.Decls {
		function := "<package>"
		if fn, ok := decl.(*ast.FuncDecl); ok {
			function = fn.Name.Name
		}
		add := func(kind string, n ast.Node) { out = append(out, outputSink{name, function, kind, normalized(n)}) }
		parents := map[ast.Node]ast.Node{}
		var stack []ast.Node
		ast.Inspect(decl, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return false
			}
			if len(stack) > 0 {
				parents[n] = stack[len(stack)-1]
			}
			stack = append(stack, n)
			return true
		})
		ast.Inspect(decl, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && imports[id.Name] == "os" && sel.Sel.Name == "NewFile" && len(v.Args) > 0 {
						if standardFD(v.Args[0]) {
							add("standard-fd-file", v)
						}
					}
				}
				if id, ok := v.Fun.(*ast.Ident); ok && (id.Name == "print" || id.Name == "println") {
					add("builtin-stream", v)
				}
				if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Write" && name == "internal/serverdiag/diagnostics.go" {
					add("sanitized-writer", v)
				}
				if name == "internal/serverruntime/run.go" && function == "Run" {
					usesOutput := false
					ast.Inspect(v, func(n ast.Node) bool {
						if id, ok := n.(*ast.Ident); ok && id.Name == "output" {
							usesOutput = true
						}
						return true
					})
					if usesOutput {
						add("cli-output", v)
					}
				}
				if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok {
						pkg := imports[id.Name]
						if pkg == "log" || pkg == "log/slog" {
							add("logger", v)
						}
						if pkg == "fmt" && (strings.HasPrefix(sel.Sel.Name, "Fprint") || strings.HasPrefix(sel.Sel.Name, "Print")) {
							add("formatted-writer", v)
						}
					}
					if sel.Sel.Name == "SetOutput" {
						add("output-adapter", v)
					}
				}
			case *ast.SelectorExpr:
				if id, ok := v.X.(*ast.Ident); ok {
					pkg := imports[id.Name]
					switch {
					case pkg == "log" || pkg == "log/slog":
						add("logger-reference", v)
					case pkg == "fmt" && (strings.HasPrefix(v.Sel.Name, "Print") || strings.HasPrefix(v.Sel.Name, "Fprint")):
						add("formatted-reference", v)
					case pkg == "net/http" && v.Sel.Name == "Server":
						add("http-server-type", v)
					case pkg == "net/http" && (v.Sel.Name == "ListenAndServe" || v.Sel.Name == "ListenAndServeTLS" || v.Sel.Name == "Serve" || v.Sel.Name == "ServeTLS"):
						add("http-listener-reference", v)
					case pkg == "syscall" && (v.Sel.Name == "Stderr" || v.Sel.Name == "Stdout"):
						add("standard-fd", v)
					case pkg == "runtime/debug" && v.Sel.Name == "PrintStack":
						add("raw-stack", v)
					}
				}
				if id, ok := v.X.(*ast.Ident); ok && imports[id.Name] == "os" && (v.Sel.Name == "Stdout" || v.Sel.Name == "Stderr") {
					var context ast.Node = v
					for parent := parents[v]; parent != nil; parent = parents[parent] {
						if _, ok := parent.(*ast.CallExpr); ok {
							context = parent
							break
						}
						if _, ok := parent.(*ast.AssignStmt); ok {
							context = parent
							break
						}
					}
					add("standard-stream", context)
				}
			case *ast.KeyValueExpr:
				if key, ok := v.Key.(*ast.Ident); ok && key.Name == "ErrorLog" {
					add("http-error-log", v)
				}
			case *ast.AssignStmt:
				for _, lhs := range v.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "ErrorLog" {
						add("http-error-log", v)
					}
				}
			}
			return true
		})
	}
	return out, nil
}

// These are exact reviewed calls, not file-wide or function-wide exemptions.
func allowedOutputSinks() map[outputSink]int {
	return map[outputSink]int{
		{"internal/serverdiag/diagnostics.go", "ErrorLog", "logger-reference", "log.Logger"}: 1,
		{"internal/serverdiag/diagnostics.go", "ErrorLog", "logger-reference", "log.New"}:    1,
		{"internal/serverruntime/run.go", "Run", "formatted-reference", "fmt.Fprintln"}:      2,
		{"internal/serverruntime/run.go", "Run", "formatted-reference", "fmt.Fprintf"}:       1,
		{"internal/serverruntime/run.go", "Run", "http-server-type", "http.Server"}:          1,

		{"internal/serverdiag/diagnostics.go", "run", "sanitized-writer", `w.Write(b)`}: 1,

		{"internal/serverruntime/run.go", "Run", "cli-output", `flags.SetOutput(output)`}:                                                                                                            1,
		{"internal/serverruntime/run.go", "Run", "cli-output", `fmt.Fprintln(output, "Usage: asb-server [--listen address] [--memory-dir path] [--trusted-proxy CIDR]...")`}:                         1,
		{"internal/serverruntime/run.go", "Run", "cli-output", `fmt.Fprintln(output, "AllowHTTP OFF behind any TLS proxy requires --trusted-proxy and a proxy that SETS X-Forwarded-Proto.")`}:       1,
		{"internal/serverruntime/run.go", "Run", "cli-output", `fmt.Fprintf(output, "asb-server %s\n", version)`}:                                                                                    1,
		{"cmd/asb-server/main.go", "main", "standard-stream", "serverruntime.Run(ctx, os.Args[1:], version, os.Stdout)"}:                                                                             1,
		{"internal/serverdiag/diagnostics.go", "init", "standard-stream", "New(os.Stderr)"}:                                                                                                          1,
		{"internal/serverdiag/diagnostics.go", "ErrorLog", "logger", `log.New(httpDiagnosticWriter{}, "", 0)`}:                                                                                       1,
		{"internal/serverruntime/run.go", "Run", "output-adapter", `flags.SetOutput(io.Discard)`}:                                                                                                    2,
		{"internal/serverruntime/run.go", "Run", "output-adapter", `flags.SetOutput(output)`}:                                                                                                        1,
		{"internal/serverruntime/run.go", "Run", "formatted-writer", `fmt.Fprintln(output, "Usage: asb-server [--listen address] [--memory-dir path] [--trusted-proxy CIDR]...")`}:                   1,
		{"internal/serverruntime/run.go", "Run", "formatted-writer", `fmt.Fprintln(output, "AllowHTTP OFF behind any TLS proxy requires --trusted-proxy and a proxy that SETS X-Forwarded-Proto.")`}: 1,
		{"internal/serverruntime/run.go", "Run", "formatted-writer", `fmt.Fprintf(output, "asb-server %s\n", version)`}:                                                                              1,
		{"internal/serverruntime/run.go", "Run", "http-error-log", `ErrorLog: serverdiag.ErrorLog()`}:                                                                                                1,
	}
}
func TestServerProductionOutputSinkGate(t *testing.T) {
	goExe := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOROOT() == "" {
		var err error
		goExe, err = exec.LookPath("go")
		if err != nil {
			t.Fatal("Go tooling unavailable")
		}
	}
	cmd := exec.Command(goExe, "list", "-deps", "-json", "./cmd/asb-server")
	cmd.Dir = "../.."
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal("cannot resolve server dependency source")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	found := map[outputSink]int{}
	count := 0
	for {
		var pkg struct {
			Dir, ImportPath                   string
			Standard                          bool
			GoFiles, CgoFiles, IgnoredGoFiles []string
			Module                            *struct{ Path, Dir string }
		}
		err := dec.Decode(&pkg)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("dependency decode failed")
		}
		if pkg.Standard || pkg.Module == nil || pkg.Module.Path != "github.com/xelexis-dev/agent-session-broker/source" {
			continue
		}
		files := append(append(pkg.GoFiles, pkg.CgoFiles...), pkg.IgnoredGoFiles...)
		seenFiles := map[string]bool{}
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") || seenFiles[name] {
				continue
			}
			seenFiles[name] = true
			path := filepath.Join(pkg.Dir, name)
			rel, err := filepath.Rel(pkg.Module.Dir, path)
			if err != nil {
				t.Fatal(err)
			}
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			sinks, err := sinkCalls(filepath.ToSlash(rel), src)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range sinks {
				found[v]++
			}
			count++
		}
	}
	if count == 0 {
		t.Fatal("empty server dependency inventory")
	}
	allowed := allowedOutputSinks()
	var failures []string
	for v, n := range found {
		if allowed[v] != n {
			failures = append(failures, v.File+":"+v.Function+" "+v.Kind+" "+v.Expression)
		}
	}
	for v, n := range allowed {
		if found[v] != n {
			failures = append(failures, "missing reviewed sink "+v.File+":"+v.Expression)
		}
	}
	sort.Strings(failures)
	if len(failures) > 0 {
		t.Fatalf("server output sinks require review:\n%s", strings.Join(failures, "\n"))
	}
}
func TestServerSinkGateRejectsNewAndAliasedSinks(t *testing.T) {
	for _, src := range []string{
		`package p; import l "log";func f(){l.Print("input")}`,
		`package p; import . "log";func f(){Print("input")}`,
		`package p;func f(){println("input")}`,
		`package p;import "fmt";func f(){fmt.Println("input")}`,
		`package p;import "net/http";func f(){_ = &http.Server{}}`,
		`package p;import f "fmt";import o "os";func f1(){f.Fprintln(o.Stderr,"input")}`,
		`package p;import "os";func f(){out:=os.Stdout;wrapper(out)}`,
		`package p;import "net/http";func f(){_ = &http.Server{ErrorLog:nil}}`,
		`package p;func f(){server.ErrorLog = logger}`,
		`package p;import "log";func f(){log.New(writer,"",0)}`,
		`package p;import "log/slog";func f(){slog.Info("input")}`,
	} {
		sinks, err := sinkCalls("internal/new.go", []byte(src))
		if err != nil || len(sinks) == 0 {
			t.Fatal("new/aliased output canary escaped gate")
		}
	}
	original := []byte(`package p;import "log";func allowed(){log.Print("safe");log.Print("extra")}`)
	sinks, err := sinkCalls("internal/new.go", original)
	if err != nil || len(sinks) != 4 {
		t.Fatal("added call in reviewed function escaped gate")
	}
}

func TestServerSinkGateRejectsExtraCallsInReviewedWriter(t *testing.T) {
	src := []byte(`package serverdiag;func(s *Sink)run(w Writer){w.Write(b);w.Write(secret)}`)
	sinks, err := sinkCalls("internal/serverdiag/diagnostics.go", src)
	if err != nil {
		t.Fatal(err)
	}
	rejected := false
	allowed := allowedOutputSinks()
	for _, sink := range sinks {
		if allowed[sink] == 0 {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("added call in reviewed writer escaped gate")
	}
	src = []byte(`package main;import "os";func main(){badWrapper(os.Stdout)}`)
	sinks, err = sinkCalls("cmd/asb-server/main.go", src)
	if err != nil || len(sinks) != 1 || allowed[sinks[0]] != 0 {
		t.Fatal("changed stream wrapper escaped gate")
	}
}

func standardFD(expr ast.Expr) bool {
	switch v := expr.(type) {
	case *ast.ParenExpr:
		return standardFD(v.X)
	case *ast.CallExpr:
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "uintptr" && len(v.Args) == 1 {
			return standardFD(v.Args[0])
		}
	case *ast.UnaryExpr:
		if v.Op == token.ADD {
			return standardFD(v.X)
		}
	case *ast.BasicLit:
		if v.Kind == token.INT {
			n, err := strconv.ParseUint(v.Value, 0, 64)
			return err == nil && (n == 1 || n == 2)
		}
	}
	return false
}
