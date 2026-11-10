package controlpanel

import (
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestTokenOwnerAlwaysIssuer(t *testing.T) {
	s := userStore(t)
	for _, actor := range []string{"owner", "admin", "developer"} {
		t.Run(actor, func(t *testing.T) {
			other := "developer"
			if actor == other {
				other = "owner"
			}
			before := len(s.ProjectsFor("admin")[0].Tokens)
			if raw, err := s.IssueTokenFor(actor, "demo", actor+"-forged", other); !errors.Is(err, ErrForbidden) || raw != "" {
				t.Error("different owner was not rejected")
			}
			if len(s.ProjectsFor("admin")[0].Tokens) != before {
				t.Error("forged request persisted a token")
			}
			for _, owner := range []string{"", actor} {
				nickname := actor + "-self-" + owner
				raw, err := s.IssueTokenFor(actor, "demo", nickname, owner)
				if err != nil {
					t.Fatalf("self issuance: %v", err)
				}
				if !s.Authorize("demo", raw) {
					t.Fatal("self-issued token cannot authenticate")
				}
				grant, _, ok := s.authorize("demo", raw)
				if !ok || grant.Claim(nickname) != nil || grant.Check(nickname) != nil {
					t.Fatal("self-issued token cannot bind and validate its session")
				}
				tokens := s.ProjectsFor("admin")[0].Tokens
				if tokens[len(tokens)-1].OwnerID != actor {
					t.Fatal("persisted owner is not issuer")
				}
			}
		})
	}
	for _, actor := range []string{"viewer", "outsider"} {
		if _, err := s.IssueTokenFor(actor, "demo", actor+"-denied", ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s issuance was not forbidden", actor)
		}
	}
}

func tokenOwnerHistory(t *testing.T) *Store {
	t.Helper()
	s := userStore(t)
	if err := s.SetProjectMember("owner", "demo", "viewer", "developer"); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"owner", "developer", "viewer"} {
		if _, err := s.IssueProjectTokenFor(actor, "demo", actor+"-history"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetProjectMember("owner", "demo", "viewer", "viewer"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTokenOwnerHistoricalVisibilityAndPersistence(t *testing.T) {
	s := tokenOwnerHistory(t)
	before := s.ProjectsFor("owner")[0].Tokens
	dir := s.root.Name()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !reflect.DeepEqual(before, s.ProjectsFor("owner")[0].Tokens) {
		t.Fatal("restart changed existing token ownership or history")
	}
	for _, actor := range []string{"owner", "admin", "developer", "viewer", "outsider"} {
		projects := s.ProjectsFor(actor)
		if actor == "outsider" {
			if len(projects) != 0 {
				t.Error("nonmember can view project tokens")
			}
			continue
		}
		want := 1
		if actor == "owner" || actor == "admin" {
			want = 3
		}
		if len(projects) != 1 || len(projects[0].Tokens) != want {
			t.Errorf("%s token visibility count is incorrect", actor)
			continue
		}
		for _, token := range projects[0].Tokens {
			if token.TokenHash != "" || (want == 1 && token.OwnerID != actor) {
				t.Errorf("%s sees another owner's token or hash", actor)
			}
			if actor == "viewer" && !token.Revoked {
				t.Error("viewer history became usable after downgrade")
			}
		}
	}
}

func TestTokenOwnerPanelIssuanceAndVisibility(t *testing.T) {
	s := tokenOwnerHistory(t)
	h := New(s, &panelBackend{}, http.NotFoundHandler())
	for _, actor := range []string{"owner", "admin", "developer", "viewer", "outsider"} {
		t.Run(actor, func(t *testing.T) {
			client := loginPanelUser(t, h, actor)
			page := client.call("GET", "/projects/settings?project=demo", nil)
			if actor == "outsider" {
				if page.Code != http.StatusForbidden {
					t.Error("nonmember can view tokens")
				}
				return
			}
			if page.Code != http.StatusOK || strings.Contains(page.Body.String(), `name="ownerID"`) {
				t.Error("token page still accepts an owner field")
			}
			for _, owner := range []string{"owner", "developer", "viewer"} {
				// Downgrading viewer revoked its token; no UI role sees it.
				want := owner != "viewer" && (actor == "owner" || actor == "admin" || actor == owner)
				if strings.Contains(page.Body.String(), ">"+owner+"-history<") != want {
					t.Errorf("visibility of %s history is incorrect", owner)
				}
			}
			if actor == "viewer" {
				if strings.Contains(page.Body.String(), `action="/tokens/project"`) || strings.Contains(page.Body.String(), `action="/tokens/revoke"`) {
					t.Error("viewer has token mutation controls")
				}
				return
			}
			issued := client.call("POST", "/tokens/project", url.Values{"project": {"demo"}, "label": {actor + "-http"}, "ownerID": {"owner"}})
			if issued.Code != http.StatusSeeOther {
				t.Fatal("project token issuance did not redirect")
			}
			issued = client.call("GET", issued.Header().Get("Location"), nil)
			if issued.Code != http.StatusOK || !strings.Contains(issued.Body.String(), `id="issued-token"`) {
				t.Error("project token issuance failed")
			}
			tokens := s.ProjectsFor("admin")[0].Tokens
			if last := tokens[len(tokens)-1]; last.Kind != tokenProject || last.OwnerID != actor {
				t.Error("HTTP issuance persisted a different owner or kind")
			}
		})
	}
}
