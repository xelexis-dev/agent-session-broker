package pathguard

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// These full strings were checked against both callers before the move.
// Parse even non-native files so unreachable descriptor failures stay frozen.
func TestPinFrozenErrorTable(t *testing.T) {
	rows := []struct {
		file           string
		index          int
		groups, memory string
	}{
		{"pin_darwin.go", 0, "open pinned directory: cause", "open pinned directory: cause"},
		{"pin_linux.go", 0, "open pinned membership directory: cause", "open pinned memory directory: cause"},
		{"pin_windows.go", 0, "open pinned membership directory: cause", "open pinned memory directory: cause"},
		{"pin_linux.go", 1, "open pinned membership directory: invalid descriptor", "open pinned memory directory: invalid descriptor"},
		{"pin_windows.go", 1, "open pinned membership directory: invalid handle", "open pinned memory directory: invalid handle"},
		{"pin.go", 0, "pinned membership directory identity changed", "pinned memory directory identity changed"},
		{"pin.go", 1, "pinned membership directory no longer matches canonical path", "pinned memory directory no longer matches canonical path"},
		{"pin_darwin.go", 1, "access pinned membership directory through directory descriptor: cause", "access pinned nodes directory through directory descriptor: cause"},
		{"pin_linux.go", 2, "access pinned membership directory through procfs: cause", "access pinned nodes directory through procfs: cause"},
		{"pin.go", 2, "invalid: membership storage is unsupported on synthetic; supported platforms are Windows and Linux", "invalid: memory storage is unsupported on synthetic; supported platforms are Windows and Linux"},
	}
	cause, invalid := errors.New("cause"), errors.New("invalid")
	for _, row := range rows {
		f, err := parser.ParseFile(token.NewFileSet(), row.file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var calls []*ast.CallExpr
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := fn.X.(*ast.Ident)
			if ok && pkg.Name == "fmt" && fn.Sel.Name == "Errorf" {
				calls = append(calls, call)
			}
			return true
		})
		if row.index >= len(calls) {
			t.Fatalf("%s missing error %d", row.file, row.index)
		}
		call := calls[row.index]
		format, err := strconv.Unquote(call.Args[0].(*ast.BasicLit).Value)
		if err != nil {
			t.Fatal(err)
		}
		for _, caller := range []struct {
			labels PinLabels
			want   string
		}{
			{PinLabels{Store: "membership", Access: "membership", Invalid: invalid}, row.groups},
			{PinLabels{Store: "memory", Access: "nodes", Invalid: invalid}, row.memory},
		} {
			var args []any
			for _, arg := range call.Args[1:] {
				switch a := arg.(type) {
				case *ast.SelectorExpr:
					switch a.Sel.Name {
					case "Store":
						args = append(args, caller.labels.Store)
					case "Access":
						args = append(args, caller.labels.Access)
					case "Invalid":
						args = append(args, caller.labels.Invalid)
					default:
						t.Fatalf("unknown label %s", a.Sel.Name)
					}
				case *ast.Ident:
					switch a.Name {
					case "err":
						args = append(args, cause)
					case "platform":
						args = append(args, "synthetic")
					default:
						t.Fatalf("unknown argument %s", a.Name)
					}
				default:
					t.Fatalf("unknown argument %T", arg)
				}
			}
			got := fmt.Errorf(format, args...)
			if got.Error() != caller.want {
				t.Fatalf("%s/%d: %q != %q", row.file, row.index, got, caller.want)
			}
			if errors.Is(got, cause) != (row.index == 0 && row.file != "pin.go" || row.file == "pin_darwin.go" && row.index == 1 || row.file == "pin_linux.go" && row.index == 2) {
				t.Fatalf("cause identity: %v", got)
			}
			if errors.Is(got, invalid) != (row.file == "pin.go" && row.index == 2) {
				t.Fatalf("invalid identity: %v", got)
			}
		}
	}
}

func TestUnsupportedPinFailsClosed(t *testing.T) {
	for _, store := range []string{"membership", "memory"} {
		invalid := errors.New("invalid")
		pin, err := unsupportedPin("synthetic", PinLabels{Store: store, Invalid: invalid})
		want := "invalid: " + store + " storage is unsupported on synthetic; supported platforms are Windows and Linux"
		if pin != nil || err.Error() != want || !errors.Is(err, invalid) {
			t.Fatalf("pin=%v err=%v", pin, err)
		}
	}
}
