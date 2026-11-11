package controlpanel

import (
	"bytes"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTokenScopeEditorGETRendersEligibleToken(t *testing.T) {
	t.Parallel()
	s := directStore(t)
	raw := directParent(t, s, 8)
	parent := scopeEditParent(t, s, raw)
	browser := loginPanelUser(t, New(s, tokenViewBackend{}, http.NotFoundHandler()), "admin")
	response := browser.call("GET", "/projects/settings?project=demo&edit="+parent.ID, nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `id="scope-editor"`) {
		t.Fatal("GET edit did not render the token scope editor")
	}
}

func TestTokenScopeEditorReviewBackNeverSweepOrWrite(t *testing.T) {
	t.Parallel()
	s, advance := churnStore(t)
	raw := directParent(t, s, 8)
	parent := scopeEditParent(t, s, raw)
	_ = churnCredentialFor(t, s, raw, 501, true)
	browser := loginPanelUser(t, New(s, tokenViewBackend{}, http.NotFoundHandler()), "admin")
	advance(directWindow + time.Second)
	before, _ := json.Marshal(s.data)
	writes := 0
	write, batch := s.write, s.writeBatch
	s.write = func(data []byte) error { writes++; return write(data) }
	s.writeBatch = func(puts map[string][]byte, deletes []string) error { writes++; return batch(puts, deletes) }
	form := url.Values{"project": {"demo"}, "id": {parent.ID}, "scope": {access.MessageSend, access.SessionRegister}, "settings_version": {scopeEditVersion(s, parent)}, "intent": {"review"}}
	reviewed := browser.call("POST", "/tokens/scopes", form)
	if reviewed.Code != 200 || !strings.Contains(reviewed.Body.String(), `data-step="review"`) {
		t.Fatal("review failed")
	}
	form.Set("intent", "back")
	back := browser.call("POST", "/tokens/scopes", form)
	after, _ := json.Marshal(s.data)
	if back.Code != 200 || !strings.Contains(back.Body.String(), `data-step="select"`) || writes != 0 || !bytes.Equal(before, after) {
		t.Fatal("review/back performed a write or expiry sweep")
	}
}

func TestTokenScopeEditorHTTPFlowAndConflict(t *testing.T) {
	t.Parallel()
	s := userStore(t)
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueProjectTokenWithOptionsFor("owner", "demo", "editor-parent", ProjectTokenOptions{Scopes: []string{access.MessageSend, access.SessionRead, access.SessionRegister}})
	if err != nil {
		t.Fatal(err)
	}
	parent := scopeEditParent(t, s, raw)
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	owner := loginPanelUser(t, h, "owner")
	form := url.Values{"project": {"demo"}, "id": {parent.ID}, "scope": {access.MessageReceive, access.SessionRegister}, "settings_version": {scopeEditVersion(s, parent)}, "intent": {"review"}}
	before, _ := json.Marshal(s.data)
	audits := len(s.audits)
	review := owner.call("POST", "/tokens/scopes", form)
	if review.Code != 200 || !strings.Contains(review.Body.String(), `data-step="review"`) || !strings.Contains(review.Body.String(), en(tokenScopeReceiveAddedHelp)) {
		t.Fatal("review did not show validated changes")
	}
	after, _ := json.Marshal(s.data)
	if !bytes.Equal(before, after) || len(s.audits) != audits {
		t.Fatal("review mutated state")
	}
	form.Set("intent", "back")
	back := owner.call("POST", "/tokens/scopes", form)
	if back.Code != 200 || !strings.Contains(back.Body.String(), `data-step="select"`) {
		t.Fatal("back lost draft")
	}
	form.Set("intent", "apply")
	applied := owner.call("POST", "/tokens/scopes", form)
	if applied.Code != 303 || applied.Header().Get("Location") != projectURL("settings", "demo") {
		t.Fatal("apply did not redirect to saved flash")
	}
	if len(s.audits) != audits+1 {
		t.Fatal("apply audit count")
	}
	duplicate := owner.call("POST", "/tokens/scopes", form)
	if duplicate.Code != 409 || len(s.audits) != audits+1 || !strings.Contains(duplicate.Body.String(), `data-step="select"`) || strings.Contains(duplicate.Body.String(), `data-step="review"`) {
		t.Fatal("duplicate apply did not return fresh editor")
	}
	current, err := s.TokenScopeEditFor("owner", "demo", parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(duplicate.Body.String(), current.Version) {
		t.Fatal("conflict lost fresh baseline version")
	}
	// No-op review and apply must not create audit records.
	form.Set("settings_version", current.Version)
	form.Set("intent", "review")
	noop := owner.call("POST", "/tokens/scopes", form)
	if noop.Code != 200 || !strings.Contains(noop.Body.String(), en("tokens.scope_noop")) || len(s.audits) != audits+1 {
		t.Fatal("no-op review")
	}
	form.Set("intent", "apply")
	if owner.call("POST", "/tokens/scopes", form).Code != 303 || len(s.audits) != audits+1 {
		t.Fatal("no-op apply")
	}
}

func TestTokenScopeEditorDenialsNeverRenderTarget(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"foreign", "viewer", "demotion", "revoked", "expired", "recreated"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			s := userStore(t)
			if err := s.SetScopeIssuanceReady("admin", true); err != nil {
				t.Fatal(err)
			}
			raw, err := s.IssueProjectTokenWithOptionsFor("owner", "demo", "private-target-label", ProjectTokenOptions{Scopes: []string{access.SessionRegister, access.MemoryWrite}})
			if err != nil {
				t.Fatal(err)
			}
			parent := scopeEditParent(t, s, raw)
			h := New(s, tokenViewBackend{}, http.NotFoundHandler())
			actor := "owner"
			if change == "foreign" {
				actor = "developer"
			}
			if change == "viewer" {
				actor = "viewer"
			}
			browser := loginPanelUser(t, h, actor)
			version := scopeEditVersion(s, parent)
			switch change {
			case "demotion":
				if err := s.SetProjectMember("admin", "demo", "developer", "owner"); err != nil {
					t.Fatal(err)
				}
				if err := s.SetProjectMember("admin", "demo", "owner", "viewer"); err != nil {
					t.Fatal(err)
				}
			case "revoked", "expired":
				p := s.data.Projects["demo"]
				for i := range p.Tokens {
					if p.Tokens[i].ID == parent.ID {
						if change == "revoked" {
							p.Tokens[i].Revoked = true
						} else {
							p.Tokens[i].ExpiresAt = s.now().Add(-time.Second)
						}
					}
				}
				if err := s.commitProject(p); err != nil {
					t.Fatal(err)
				}
			case "recreated":
				// A recreated generation cannot contain the old parent ID.
				p := s.data.Projects["demo"]
				p.ProjectGenerationID = "replacement-generation"
				p.Tokens = nil
				s.data.Projects["demo"] = p
			}
			for _, intent := range []string{"review", "back", "apply"} {
				response := browser.call("POST", "/tokens/scopes", url.Values{"project": {"demo"}, "id": {parent.ID}, "scope": {access.SessionRegister}, "settings_version": {version}, "intent": {intent}})
				if response.Code != 403 || strings.Contains(response.Body.String(), "private-target-label") || strings.Contains(response.Body.String(), access.MemoryWrite) {
					t.Fatal("denied request exposed target or rendered stale editor")
				}
			}
			got := browser.call("GET", "/projects/settings?project=demo&edit="+parent.ID, nil)
			if got.Code != 403 || strings.Contains(got.Body.String(), "private-target-label") {
				t.Fatal("denied GET exposed target")
			}
		})
	}
}

func TestTokenScopeEditorInvalidAndHistoryReset(t *testing.T) {
	t.Parallel()
	s := directStore(t)
	raw := directParent(t, s, 8)
	parent := scopeEditParent(t, s, raw)
	child := churnCredentialFor(t, s, raw, 601, true)
	p := s.data.Projects["demo"]
	for i := range p.Tokens {
		if p.Tokens[i].TokenHash == digest(child.raw) {
			p.Tokens[i].Revoked = true
			p.Tokens[i].Scopes = []string{access.MessageReceive}
		}
	}
	if err := s.commitProject(p); err != nil {
		t.Fatal(err)
	}
	browser := loginPanelUser(t, New(s, tokenViewBackend{}, http.NotFoundHandler()), "admin")
	for _, intent := range []string{"review", "back", "apply"} {
		response := browser.call("POST", "/tokens/scopes", url.Values{"project": {"demo"}, "id": {parent.ID}, "scope": {access.SessionRegister}, "settings_version": {scopeEditVersion(s, parent)}, "intent": {intent}})
		if response.Code != 400 || !strings.Contains(response.Body.String(), en(tokenScopeHistoryMessage)) || !strings.Contains(response.Body.String(), en("tokens.revoking_the_token_also_revokes_its_existing")) || !strings.Contains(response.Body.String(), `data-step="select"`) {
			t.Fatal("history error did not reset editor")
		}
	}
	response := browser.call("POST", "/tokens/scopes", url.Values{"project": {"demo"}, "id": {parent.ID}, "scope": {"unknown"}, "settings_version": {scopeEditVersion(s, parent)}, "intent": {"review"}})
	if response.Code != 400 || !strings.Contains(response.Body.String(), en(tokenScopeEditorInvalidMessage)) {
		t.Fatal("invalid editor request uses wrong notice")
	}
}

func TestTokenScopeEditorAuditProjectionEscapesCommittedSnapshot(t *testing.T) {
	t.Parallel()
	s := directStore(t)
	raw := directParent(t, s, 8)
	parent := scopeEditParent(t, s, raw)
	_ = churnCredentialFor(t, s, raw, 602, true)
	p := s.data.Projects["demo"]
	for i := range p.Tokens {
		if p.Tokens[i].ID == parent.ID {
			p.Tokens[i].Label = `<script>snapshot</script>`
		}
	}
	if err := s.commitProject(p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditTokenScopesFor("admin", "demo", parent.ID, []string{access.MessageSend, access.SessionRegister}, []string{scopeEditVersion(s, parent)}); err != nil {
		t.Fatal(err)
	}
	view, err := s.directAuditViewFor("admin", "demo", "en")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range view.Rows {
		if row.Kind == tokenScopesAuditKind {
			found = true
			if row.Event != en("audit.event.scopes") || row.Name != `<script>snapshot</script>` || row.Parent != row.Name || row.Registration != en("audit.not_applicable") || row.SessionURL != "" || !strings.HasSuffix(row.Reason, "Credentials changed: 1") {
				t.Fatal("scope audit projection lost committed fields")
			}
		}
	}
	if !found {
		t.Fatal("missing scope audit")
	}
	browser := loginPanelUser(t, New(s, tokenViewBackend{}, http.NotFoundHandler()), "admin")
	page := browser.call("GET", "/projects/audit?project=demo", nil)
	if page.Code != 200 || strings.Contains(page.Body.String(), `<script>snapshot</script>`) || !strings.Contains(page.Body.String(), `&lt;script&gt;snapshot&lt;/script&gt;`) {
		t.Fatal("audit snapshot not escaped")
	}
	s.data.ScopeIssuanceReady = false
	off, err := s.directAuditViewFor("admin", "demo", "en")
	if err != nil || off.Enabled {
		t.Fatal("existing audit readiness gate changed")
	}
}

func TestTokenScopeEditorNarrowingOffAndImmutableMetadata(t *testing.T) {
	t.Parallel()
	s := userStore(t)
	if err := s.SetScopeIssuanceReady("admin", true); err != nil {
		t.Fatal(err)
	}
	raw, err := s.IssueProjectTokenWithOptionsFor("owner", "demo", "automatic-parent", ProjectTokenOptions{Scopes: []string{access.MessageReceive, access.SessionRegister, access.SessionRegisterAuto}, MaxAutoBindings: 10, AutoLifetimeDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	parent := scopeEditParent(t, s, raw)
	h := New(s, tokenViewBackend{}, http.NotFoundHandler())
	owner := loginPanelUser(t, h, "owner")
	page := owner.call("GET", "/projects/settings?project=demo&edit="+parent.ID, nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), en("tokens.automatic_registration_limit")+" "+en("tokens.identities", "n", 10)+" "+en("tokens.lifetime")+" "+en("tokens.days", "n", 7)) {
		t.Fatal("immutable metadata not rendered")
	}
	if err := s.SetScopeIssuanceReady("admin", false); err != nil {
		t.Fatal(err)
	}
	for _, language := range []string{"en", "ko"} {
		if err := s.UpdateLanguage("owner", language); err != nil {
			t.Fatal(err)
		}
		page = owner.call("GET", "/projects/settings?project=demo&edit="+parent.ID, nil)
		if page.Code != 200 || !strings.Contains(page.Body.String(), localText(language, "tokens.restricted_token_issuance_is_off_you_can")) {
			t.Fatalf("%s: off-state narrow editor unavailable", language)
		}
	}
	if err := s.UpdateLanguage("owner", "en"); err != nil {
		t.Fatal(err)
	}
	current, err := s.TokenScopeEditFor("owner", "demo", parent.ID)
	if err != nil || current.CanWiden {
		t.Fatal("off-state widening allowed")
	}
	proposal := []string{access.SessionRegister}
	form := url.Values{"project": {"demo"}, "id": {parent.ID}, "scope": proposal, "settings_version": {current.Version}, "intent": {"review"}}
	preview := owner.call("POST", "/tokens/scopes", form)
	if preview.Code != 200 || !strings.Contains(preview.Body.String(), enHTML(tokenScopeReceiveRemovedHelp)) {
		t.Fatal("off-state narrowing review")
	}
	for _, bad := range []string{"auto_cap", "auto_days", "owner", "admission_mode"} {
		forged := url.Values{}
		for key, values := range form {
			forged[key] = append([]string(nil), values...)
		}
		forged.Set(bad, "1")
		response := owner.call("POST", "/tokens/scopes", forged)
		if response.Code != 400 || !strings.Contains(response.Body.String(), en(tokenScopeEditorInvalidMessage)) {
			t.Fatal("immutable field injection accepted")
		}
	}
	form.Set("intent", "apply")
	if owner.call("POST", "/tokens/scopes", form).Code != 303 {
		t.Fatal("off-state narrowing apply")
	}
	after, err := s.TokenScopeEditFor("owner", "demo", parent.ID)
	if err != nil || !after.Auto || after.MaxAutoBindings != 10 || after.LifetimeDays != 7 || !access.HasScope(after.Scopes, access.SessionRegisterAuto) {
		t.Fatal("scope edit altered immutable admission")
	}
}
