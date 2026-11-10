package controlpanel

import (
	"net/url"
	"strings"
	"testing"
)

func TestProjectTokenIssuanceWithoutSubmissionToken(t *testing.T) {
	s, _, h := misuseFixture(t)
	c := loginPanelUser(t, h, "owner")
	// Call the handler directly: userPanelClient.call injects a browser form nonce.
	w := misuseRequest(h, c.cookie, "POST", "/tokens/project", url.Values{"csrf": {c.csrf}, "project": {"demo"}, "label": {"legacy caller"}})
	if w.Code != 303 {
		t.Fatalf("legacy issuance status=%d, want 303", w.Code)
	}
	count := 0
	for _, p := range s.ProjectsFor("owner") {
		if p.Name == "demo" {
			count = len(p.Tokens)
		}
	}
	if count != 1 {
		t.Fatalf("issued token count=%d, want 1", count)
	}
}

func TestProjectTokenOptionalSubmissionValidationAndCSRF(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		csrf   string
		status int
	}{
		{"missing csrf, absent submission", nil, "", 403},
		{"wrong csrf, absent submission", nil, "wrong", 403},
		{"missing csrf, present submission", []string{"invalid"}, "", 403},
		{"wrong csrf, present submission", []string{"invalid"}, "wrong", 403},
		{"explicit empty submission", []string{""}, "valid", 409},
		{"unknown submission", []string{"invalid"}, "valid", 409},
		{"multiple submissions", []string{"one", "two"}, "valid", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, b, h := misuseFixture(t)
			c := loginPanelUser(t, h, "owner")
			form := url.Values{"project": {"demo"}, "label": {"compat fixture"}}
			if tc.csrf == "valid" {
				form.Set("csrf", c.csrf)
			} else if tc.csrf != "" {
				form.Set("csrf", tc.csrf)
			}
			if tc.values != nil {
				form["issuance_token"] = tc.values
			}
			before := misuseSnapshot(t, s)
			w := misuseRequest(h, c.cookie, "POST", "/tokens/project", form)
			if w.Code != tc.status {
				t.Fatalf("status=%d, want %d", w.Code, tc.status)
			}
			assertMisuseUnchanged(t, s, b, before)
			if tc.status == 409 && !strings.Contains(w.Body.String(), `name="issuance_token" value="`) {
				t.Fatal("rejected form lacks fresh submission field")
			}
		})
	}
}
