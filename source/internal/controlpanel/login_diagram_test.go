package controlpanel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The login diagram shows Claude and Codex each connected to Broker and
// Broker connected to Project, as in the revised Figma frame and the catalog.
func TestLoginDiagramShowsHarnessesIntoBrokerIntoProject(t *testing.T) {
	h := New(userStore(t), &panelBackend{}, http.NotFoundHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://broker/login", nil))
	body := w.Body.String()
	start := strings.Index(body, `<figure class="diagram"`)
	end := strings.Index(body, `</figure>`)
	if start < 0 || end < start {
		t.Fatal("login page has no diagram")
	}
	diagram := body[start:end]
	for _, want := range []string{
		`aria-describedby="login-diagram-description"`,
		`<svg class="diagram__edges" viewBox="0 0 604 208" aria-hidden="true" focusable="false"`,
		`<path d="M96 48 H172 V88 H254"/><path d="M247 81 L254 88 L247 95"/>`,
		`<path d="M96 160 H196 V120 H254"/><path d="M247 113 L254 120 L247 127"/>`,
		`<path d="M350 104 H508"/><path d="M501 97 L508 104 L501 111"/>`,
		`<p class="sr-only" id="login-diagram-description">Claude와 Codex가 각각 Broker로 연결되고, Broker가 Project로 연결됩니다.</p>`,
	} {
		if !strings.Contains(diagram, want) {
			t.Errorf("diagram lacks %s", want)
		}
	}
	names := []string{}
	for _, part := range strings.Split(diagram, `<span class="diagram__name">`)[1:] {
		names = append(names, part[:strings.Index(part, "<")])
	}
	if strings.Join(names, ",") != "Claude,Codex,Broker,Project" {
		t.Errorf("diagram nodes = %v", names)
	}
	if strings.Count(diagram, "<path ") != 6 || strings.Contains(diagram, "공유 메모리") || strings.Contains(diagram, "style=") {
		t.Error("diagram has extra edges, the old memory node, or inline style")
	}
}
