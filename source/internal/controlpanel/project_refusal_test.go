package controlpanel

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProjectRecreatedRefusesOldGenerationWithoutDisclosure(t *testing.T) {
	f := newDeviceHTTPFixture(t)
	old := f.store.Projects()[0].ProjectGenerationID
	f.store.mu.Lock()
	p := f.store.data.Projects["demo"]
	p.ProjectGenerationID = rand.Text()
	p.Enabled = false
	p.Tokens = nil
	f.store.data.Projects["demo"] = p
	f.store.mu.Unlock()
	for _, path := range []string{"/mcp", deviceAuthorizationPath, deviceTokenPath, directPath} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path+"?project=demo", strings.NewReader("client_id=asb-client-v1"))
			req.Header.Set("Authorization", "Bearer synthetic-revoked")
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("X-ASB-Project-Generation", old)
			req.Header.Set("X-ASB-Client-Transport", "http")
			w := httptest.NewRecorder()
			f.h.ServeHTTP(w, req)
			var body map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != 410 || body["error"] != "project_deleted" {
				t.Fatalf("old generation must stop, got status %d code %v", w.Code, body["error"])
			}
			if strings.Contains(w.Body.String(), p.ProjectGenerationID) || w.Header().Get("X-ASB-Project-Generation") != "" {
				t.Fatal("refusal disclosed current generation")
			}
		})
	}
}

func TestProjectRefusalClassificationAndAuthorizedStaleHint(t *testing.T) {
	s := directStore(t)
	token, err := s.IssueToken("demo", "legacy-session")
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, tokenViewBackend{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	request := func(hint string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://broker/mcp?project=demo", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-ASB-Project-Generation", hint)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if request(strings.Repeat("Z", 26)).Code != 204 {
		t.Fatal("stale hint defeated valid authorization")
	}
	generation := s.data.Projects["demo"].ProjectGenerationID
	for _, test := range []struct {
		exists, enabled, deleting bool
		hint, code                string
		status                    int
	}{
		{true, false, false, generation, "project_disabled", 403},
		{true, false, true, generation, "project_deleting", 403},
		{true, false, true, strings.Repeat("Z", 26), "project_deleted", 410},
		{false, false, false, generation, "project_not_found", 404},
		{false, false, false, "", "project_not_found", 404},
	} {
		s.mu.Lock()
		p := s.data.Projects["demo"]
		p.Enabled = test.enabled
		p.Deleting = test.deleting
		if test.exists {
			s.data.Projects["demo"] = p
		} else {
			delete(s.data.Projects, "demo")
		}
		s.mu.Unlock()
		w := request(test.hint)
		var out map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != test.status || out["error"] != test.code {
			t.Fatalf("classification: %d %s", w.Code, out["error"])
		}
		if strings.Contains(w.Body.String(), generation) {
			t.Fatal("generation disclosed")
		}
	}
}
