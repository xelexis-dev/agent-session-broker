package controlpanel

import (
	"bytes"
	"strings"
	"testing"
)

func TestTokenFormExplainsExplicitConnectionRequest(t *testing.T) {
	var html bytes.Buffer
	if err := pages.ExecuteTemplate(&html, "page", uiCopyPreview("settings")); err != nil {
		t.Fatal(err)
	}
	body := html.String()
	var help = en("tokens.auto_registration_help")
	if !strings.Contains(body, "<small>"+help+"</small>") {
		t.Error("checkbox help must contain exactly the agreed two sentences")
	}
	for _, want := range []string{en("tokens.scope_session_register_auto"), strings.SplitAfterN(help, ". ", 2)[0], strings.SplitAfterN(help, ". ", 2)[1], "session:register:auto " + en("tokens.owner_or_instance_administrator_only"), "data-token-mode>" + en("tokens.scope_session_register_auto")} {
		if !strings.Contains(body, want) {
			t.Errorf("token form missing %q", want)
		}
	}
}
