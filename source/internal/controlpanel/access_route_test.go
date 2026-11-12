package controlpanel

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestAccessRouteUsesOnlyConstantTemplates(t *testing.T) {
	for _, tc := range []struct{ path, want string }{{"/mcp", "/mcp"}, {"/mcp/../secret", "{unmatched}"}, {"/unknown-secret", "{unmatched}"}, {"/attachments/secret", "/attachments/{id}"}, {"/assets/secret", "/assets/{file}"}, {"/api/v1/projects/secret/tokens/secret", "/api/v1/projects/{project}/tokens/{id}"}, {"/api/secret", "/api/{unmatched}"}} {
		r := httptest.NewRequest("OTHER", tc.path+"?q=secret", nil)
		if got := AccessRoute(r); got != tc.want {
			t.Fatalf("route template mismatch: got %q", got)
		}
	}
	if staticAPIRoute("secret") != "/api/{unmatched}" {
		t.Fatal("audit fallback exposed unknown route")
	}
}
func TestAccessRouteCoversEveryPanelDispatchPath(t *testing.T) {
	src, err := os.ReadFile("http.go")
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "http.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	routeValues := panelRouteValues(t)
	registered := map[string]bool{}
	for _, value := range panelRoutes {
		registered[value] = true
	}
	// Fixed path literals in serve must be shared constants; prefixes are deliberately templates.
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "serve" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				if path, found := routeValues[id.Name]; found && !registered[path] {
					t.Error("dispatch route missing from shared table")
				}
			}
			if v, ok := n.(*ast.BasicLit); ok && v.Kind == token.STRING {
				value, _ := strconv.Unquote(v.Value)
				if strings.HasPrefix(value, "/") && value != "/api/" && value != "/attachments/" && value != "/assets/" {
					t.Error("unshared fixed route literal in serve")
				}
			}
			return true
		})
	}
	for _, path := range panelRoutes {
		if got := AccessRoute(httptest.NewRequest("GET", path, nil)); got != path {
			t.Fatal("panel route missing from classifier")
		}
	}
}
func TestTransportDenialNeverLogsPathProjectOrHeaderCanaries(t *testing.T) {
	output := captureDiagnostics(t)
	p := &panel{}
	secret := "fixture-request-secret"
	r := httptest.NewRequest("POST", "/attachments/"+secret+"?project="+secret, strings.NewReader(secret))
	r.Host = secret
	r.RemoteAddr = "[fe80::1%" + secret + "]:1234"
	for _, name := range []string{"Authorization", "Cookie", "User-Agent", "X-Forwarded-For", "Forwarded", "X-Forwarded-Proto", "X-Other"} {
		r.Header.Set(name, secret)
	}
	p.denyTransport(httptest.NewRecorder(), r, secret)
	if text := output.String(); strings.Contains(text, secret) || !strings.Contains(text, "/attachments/{id}") {
		t.Fatal("transport diagnostic exposed unclassified input")
	}
}

// Resolve the source constants so inventory tests still inspect real dispatch uses.
func panelRouteValues(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "access_route.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, decl := range f.Decls {
		g, ok := decl.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			v, ok := spec.(*ast.ValueSpec)
			if !ok || len(v.Names) != 1 || len(v.Values) != 1 {
				t.Fatal("unsupported route constant declaration")
			}
			lit, ok := v.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Fatal("route constant must be a string")
			}
			path, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			values[v.Names[0].Name] = path
		}
	}
	return values
}

func TestTransportHintOnlyForUntrustedForwardedHeader(t *testing.T) {
	for _, tc := range []struct{ forwarded, trusted, want bool }{{false, false, false}, {true, false, true}, {true, true, false}} {
		t.Run(strconv.FormatBool(tc.forwarded)+strconv.FormatBool(tc.trusted), func(t *testing.T) {
			output := captureDiagnostics(t)
			p := &panel{}
			if tc.trusted {
				p.trustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
			}
			r := httptest.NewRequest("POST", "/mcp", nil)
			r.RemoteAddr = "127.0.0.1:1234"
			if tc.forwarded {
				r.Header.Set("X-Forwarded-Proto", "http")
			}
			p.denyTransport(httptest.NewRecorder(), r, "")
			if strings.Contains(output.String(), "--trusted-proxy") != tc.want {
				t.Error("forwarded hint does not match verdict")
			}
		})
	}
}
