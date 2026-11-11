package controlpanel

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAdminDTOAllowlistAndOwnership(t *testing.T) {
	s := userStore(t)
	for _, actor := range []string{"owner", "developer"} {
		if _, err := s.IssueProjectTokenFor(actor, "demo", actor+"-visible"); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	p := s.data.Projects["demo"]
	p.AutoBindings = []AutoBinding{{OwnerID: "owner", Identity: "private-bound-identity", HandoverSource: "private-handover-source"}}
	p.AutoExchanges = []AutoExchange{{RequestID: "private-request-id", Identity: "private-exchange-identity"}}
	for i := range p.Tokens {
		p.Tokens[i].BoundIdentity = "private-token-identity"
		p.Tokens[i].TokenHash = "private-credential-hash"
	}
	s.data.Projects["demo"] = p
	s.mu.Unlock()
	for _, actor := range []string{"viewer", "developer"} {
		t.Run(actor, func(t *testing.T) {
			projects := s.projectProjectionsFor(actor)
			members, err := s.memberProjectionsFor(actor, "demo")
			if err != nil {
				t.Fatal(err)
			}
			tokens, err := s.projectTokenProjectionsFor(actor, "demo")
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if actor == "developer" {
				want = 1
			}
			if len(tokens) != want {
				t.Fatal("token ownership disclosure changed")
			}
			for _, token := range tokens {
				if token.OwnerID != actor {
					t.Fatal("another user's token disclosed")
				}
			}
			sessions, err := s.sessionProjectionsFor(actor, "demo", []Session{{ID: "registration", SessionID: "private-session-identity", Name: "visible-session", OwnerID: "owner", Status: "alive"}})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(struct {
				Projects []projectDTO
				Members  []memberDTO
				Tokens   []projectTokenDTO
				Sessions []sessionRowDTO
			}{projects, members, tokens, sessions})
			if err != nil {
				t.Fatal("projection encoding failed")
			}
			for _, forbidden := range []string{"private-", "autoBindings", "autoExchanges", "boundIdentity", "tokenHash", "requestID", "Password", "Rebinds", "Devices"} {
				if strings.Contains(string(raw), forbidden) {
					t.Fatal("projection exposed internal field")
				}
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(mustJSON(t, projects[0]), &obj); err != nil {
				t.Fatal(err)
			}
			allowed := map[string]bool{"name": true, "generation": true, "mode": true, "enabled": true, "deleting": true, "role": true, "updated_at": true, "version": true}
			for key := range obj {
				if !allowed[key] {
					t.Fatal("project DTO gained unreviewed field")
				}
			}
		})
	}
}
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal("projection encoding failed")
	}
	return b
}

func TestAuditDTOUsesExistingFilteredPanelRows(t *testing.T) {
	s, _ := newDirectAuditPanelFixture(t)
	for _, actor := range []string{"owner", "developer", "viewer"} {
		view, viewErr := s.directAuditViewBucketFor(actor, "demo", "current", "en")
		out, err := s.projectAuditProjectionsFor(actor, "demo", "current")
		if (err == nil) != (viewErr == nil) {
			t.Fatal("audit permission changed")
		}
		if err != nil {
			continue
		}
		if len(out) != len(view.Rows) {
			t.Fatal("audit projection changed visibility")
		}
		for i, row := range view.Rows {
			for _, value := range []any{out[i], registrationAuditProjection(row)} {
				raw := string(mustJSON(t, value))
				for _, forbidden := range []string{"binding", "Identity", "Hash", "Request", "handover", "management", "tokenScopes"} {
					if strings.Contains(raw, `"`+forbidden+`":`) { // field names; labels such as "Request" are values
						t.Fatal("audit nested internal field exposed")
					}
				}
			}
		}
	}
}
