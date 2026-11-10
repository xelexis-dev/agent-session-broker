package controlpanel

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func retirementFixture(t *testing.T) (*Store, access.Grant, access.Grant, access.RetirementTarget, string) {
	t.Helper()
	s := directStore(t)
	parent := directParent(t, s, 8)
	managerRaw, err := s.IssueToken("demo", "manager")
	if err != nil {
		t.Fatal(err)
	}
	manager := directGrant(t, s, managerRaw)
	oldRaw, err := s.IssueToken("demo", "X")
	if err != nil {
		t.Fatal(err)
	}
	old := directGrant(t, s, oldRaw)
	identity := `["codex","synthetic-session","client"]`
	if err := old.Claim(identity); err != nil {
		t.Fatal(err)
	}
	if err := old.Connected(identity); err != nil {
		t.Fatal(err)
	}
	target := access.RetirementTarget{Identity: identity, Name: "X", RecordedOwner: "admin", RegistrationID: "RETIRE-REG", Event: protocol.ManagementEvent{Kind: "detached", Retired: true, ActorName: "manager", TargetName: "X", TargetRegistrationID: "RETIRE-REG", At: s.now().UTC()}}
	return s, manager, old, target, parent
}
func TestRetirementAtomicPolicyAndPreservesReplacement(t *testing.T) {
	for _, mode := range []string{"registration", "name"} {
		t.Run(mode, func(t *testing.T) {
			s, manager, old, target, parent := retirementFixture(t)
			replacementRaw, err := s.IssueToken("demo", "replacement")
			if err != nil {
				t.Fatal(err)
			}
			replacement := directGrant(t, s, replacementRaw)
			// Deliberately same name as an unbound replacement in registration mode.
			p := s.data.Projects["demo"]
			for i, tok := range p.Tokens {
				if tok.ID == replacement.TokenID {
					p.Tokens[i].Nickname = "X"
				}
			}
			s.data.Projects["demo"] = p
			if mode == "name" {
				target = access.RetirementTarget{Name: "X"}
				p = s.data.Projects["demo"]
				for i, tok := range p.Tokens {
					if tok.ID == replacement.TokenID {
						p.Tokens[i].Nickname = "replacement"
					}
				}
				s.data.Projects["demo"] = p
			}
			changed := make(chan struct{})
			s.changes["demo"] = changed
			revoked, err := manager.Retire(target, func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if len(revoked) != 1 || revoked[0].ID != old.TokenID || revoked[0].FirstConnectedAt.IsZero() {
				t.Fatal("wrong captured grants", revoked)
			}
			if err := old.Check(target.Identity); err == nil {
				t.Fatal("old token accepted")
			}
			if err := s.directPreflight("demo", parent); err != nil {
				t.Fatal("project token revoked")
			}
			if _, _, ok := s.authorize("demo", replacementRaw); !ok {
				t.Fatal("unbound replacement revoked")
			}
			select {
			case <-manager.Revoked:
				t.Fatal("caller revoked")
			default:
			}
			select {
			case <-changed:
				t.Fatal("project-wide cancellation")
			default:
			}
			if mode == "registration" {
				if len(s.audits) == 0 {
					t.Fatal("audit absent")
				}
				found := false
				for _, a := range s.audits {
					if a.Management != nil && a.Management.Retired {
						found = true
						if !validAudit(a) {
							t.Fatal("legacy audit validator rejected detached+retired")
						}
					}
				}
				if !found {
					t.Fatal("retired audit absent")
				}
			}
		})
	}
}
func TestRetirementPolicyRefusalsAndAtomicWriteFailure(t *testing.T) {
	for _, scenario := range []string{"unused-bound", "unused-unbound", "forbidden", "storage", "detach"} {
		t.Run(scenario, func(t *testing.T) {
			s, manager, old, target, _ := retirementFixture(t)
			detachCalled := false
			switch scenario {
			case "unused-bound", "unused-unbound":
				target = access.RetirementTarget{Name: "X"}
				p := s.data.Projects["demo"]
				for i, tok := range p.Tokens {
					if tok.ID == old.TokenID {
						p.Tokens[i].FirstConnectedAt = time.Time{}
						if scenario == "unused-unbound" {
							p.Tokens[i].BoundIdentity = ""
						}
					}
				}
				s.data.Projects["demo"] = p
			case "forbidden":
				if err := s.CreateUser("admin", "other", "synthetic-password", "user"); err != nil {
					t.Fatal(err)
				}
				if err := s.SetProjectMember("admin", "demo", "other", "developer"); err != nil {
					t.Fatal(err)
				}
				raw, err := s.IssueTokenFor("other", "demo", "other", "other")
				if err != nil {
					t.Fatal(err)
				}
				manager = directGrant(t, s, raw)
				target = access.RetirementTarget{Name: "X"}
			case "storage":
				s.writeBatch = func(map[string][]byte, []string) error { return errors.New("injected audit failure") }
			}
			before, _ := json.Marshal(s.data)
			_, err := manager.Retire(target, func() error {
				detachCalled = true
				if scenario == "detach" {
					return errors.New("detach failure")
				}
				return nil
			})
			var rejection protocol.RetireRejection
			if !errors.As(err, &rejection) {
				t.Fatal("missing rejection", err)
			}
			want := "storage"
			if scenario == "forbidden" {
				want = "forbidden"
			}
			if scenario == "unused-bound" || scenario == "unused-unbound" {
				want = "credential-unused"
			}
			if rejection.Code != want {
				t.Fatal(rejection.Code, want)
			}
			after, _ := json.Marshal(s.data)
			if string(before) != string(after) {
				t.Fatal("failed policy commit changed state")
			}
			if (scenario == "forbidden" || scenario == "unused-bound" || scenario == "unused-unbound") && detachCalled {
				t.Fatal("refusal detached target")
			}
		})
	}
}

func TestRetirementReleasesOnlyCapturedDirectBindings(t *testing.T) {
	s, manager, _, _, parent := retirementFixture(t)
	var target access.RetirementTarget
	var targetID, siblingID string
	remote := broker.NewRemoteService()
	defer remote.Close()
	remote.SetAdmission(directAdmissionFixture{})
	for i := 0; i < 2; i++ {
		r := directRequest(directForm())
		if i == 1 {
			r.SessionID = "22222222-2222-4222-8222-222222222222"
			r.RequestID = "sibling-request"
		}
		issued, err := s.ExchangeDirect("demo", parent, r, DirectProof{Absent: true}, "fixture", false)
		if err != nil {
			t.Fatal(err)
		}
		g := directGrant(t, s, issued.AccessToken)
		req := directRegisterRequest()
		req.SessionID = r.SessionID
		req.RequestID = r.RequestID
		req.ChannelID += fmt.Sprint(i)
		registered, err := remote.RegisterSession(access.WithGrant(t.Context(), g), req)
		if err != nil || registered.Code != "" {
			t.Fatal(err, registered.Code)
		}
		if err := confirmDirect(t, g, registered); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			targetID = g.TokenID
			target = access.RetirementTarget{Identity: r.identity(), Name: registered.Session.Name, RecordedOwner: "admin", RegistrationID: registered.Session.RegistrationID, Event: protocol.ManagementEvent{Kind: "detached", Retired: true, ActorName: "manager", TargetName: registered.Session.Name, TargetRegistrationID: registered.Session.RegistrationID, At: s.now().UTC()}}
		} else {
			siblingID = g.TokenID
		}
	}
	// An issued successor has no successful registration evidence and must survive name mode.
	r := directRequest(directForm())
	r.SessionID = "33333333-3333-4333-8333-333333333333"
	r.RequestID = "unused-successor"
	unused, err := s.ExchangeDirect("demo", parent, r, DirectProof{Absent: true}, "fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Retire(target, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	p := s.data.Projects["demo"]
	for _, b := range p.AutoBindings {
		if b.GrantID == targetID {
			t.Fatal("target binding retained")
		}
	}
	sibling, successor := false, false
	for _, b := range p.AutoBindings {
		sibling = sibling || b.GrantID == siblingID
		successor = successor || b.GrantID == unused.GrantID
	}
	if !sibling || !successor {
		t.Fatal("unrelated direct bindings lost")
	}
	for _, e := range p.AutoExchanges {
		if e.GrantID == targetID {
			t.Fatal("replay exchange retained")
		}
	}
	// Repeat after record-removal failure: no duplicate completed management audit.
	if _, err := manager.Retire(target, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range s.audits {
		if e.Management != nil && e.Management.Retired && e.Management.TargetRegistrationID == target.RegistrationID {
			n++
		}
	}
	if n != 1 {
		t.Fatal("duplicate retirement audit", n)
	}
}
