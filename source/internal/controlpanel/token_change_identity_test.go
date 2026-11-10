package controlpanel

import (
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"net/http"
	"testing"
)

// Characterize the current ownership boundary independently of the client gate.
func TestTokenChangeDirectOwnerBoundary(t *testing.T) {
	for _, manual := range []bool{false, true} {
		for _, differentOwner := range []bool{false, true} {
			t.Run(fmt.Sprintf("manual=%t/differentOwner=%t", manual, differentOwner), func(t *testing.T) {
				s := directStore(t)
				if s.CreateUser("admin", "other", testPassword, "user") != nil || s.SetProjectMember("admin", "demo", "other", "owner") != nil {
					t.Fatal("owner fixture")
				}
				r := directRequest(directForm())
				var child string
				if manual {
					parent, err := s.IssueProjectTokenFor("admin", "demo", "manual")
					if err != nil {
						t.Fatal(err)
					}
					code, err := s.BeginDeviceAuthorization("demo", parent, r.DeviceRequest)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = s.ApproveDeviceAuthorization("admin", code.UserCode, "worker", false); err != nil {
						t.Fatal(err)
					}
					grant, err := s.ExchangeDeviceToken("demo", parent, code.DeviceCode, r.ClientID, false)
					if err != nil {
						t.Fatal(err)
					}
					child = grant.AccessToken
				} else {
					grant, err := s.ExchangeDirect("demo", directParent(t, s, 8), r, DirectProof{}, "peer", false)
					if err != nil {
						t.Fatal(err)
					}
					child = grant.AccessToken
				}
				root := t.TempDir()
				remote, err := broker.NewSQLitePersistentRemoteService(root)
				if err != nil {
					t.Fatal(err)
				}
				remote.SetAdmission(directAdmissionFixture{})
				req := directRegisterRequest()
				g := directGrant(t, s, child)
				original, err := remote.RegisterSession(access.WithGrant(t.Context(), g), req)
				if err != nil || original.Code != "" {
					t.Fatal("initial registration")
				}
				if !manual && confirmDirect(t, g, original) != nil {
					t.Fatal("confirmation")
				}
				if err = remote.Close(); err != nil {
					t.Fatal(err)
				}
				remote, err = broker.NewSQLitePersistentRemoteService(root)
				if err != nil {
					t.Fatal(err)
				}
				defer remote.Close()
				remote.SetAdmission(directAdmissionFixture{})
				owner := "admin"
				if differentOwner {
					owner = "other"
				}
				next, err := s.IssueProjectTokenWithOptionsFor(owner, "demo", "replacement", ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto)})
				if err != nil {
					t.Fatal(err)
				}
				h := New(s, directBrokerBackend{remote}, http.NotFoundHandler())
				form := directForm()
				form.Set("request_id", "after-token-change")
				form.Set("requested_nickname", "")
				form.Set("registration_id", original.Session.RegistrationID)
				form.Set("resume_secret", req.Secret)
				w := directPOST(t, h, next, form)
				want := http.StatusOK
				if differentOwner {
					want = http.StatusConflict
				}
				if w.Code != want {
					t.Fatalf("replacement admission status=%d want=%d", w.Code, want)
				}
				form.Set("request_id", "fresh-without-proof")
				form.Del("registration_id")
				form.Del("resume_secret")
				if directPOST(t, h, next, form).Code != http.StatusConflict {
					t.Fatal("existing identity admitted without proof")
				}
				sessions, err := remote.ListSessions(t.Context())
				if err != nil || len(sessions) != 1 || sessions[0].RegistrationID != original.Session.RegistrationID {
					t.Fatal("registration duplicated or replaced")
				}
				if differentOwner {
					backend := round3LiveBackend{remote: remote, store: s}
					if err := backend.Detach(t.Context(), "demo", original.Session.RegistrationID); err != nil {
						t.Fatal(err)
					}
					form.Set("request_id", "after-admin-detach-old-proof")
					form.Set("registration_id", original.Session.RegistrationID)
					form.Set("resume_secret", req.Secret)
					w := directPOST(t, h, next, form)
					if w.Code != http.StatusConflict {
						t.Fatal("removed proof was not rejected")
					}
					form.Del("registration_id")
					form.Del("resume_secret")
					form.Set("request_id", "after-admin-detach-fresh")
					form.Set("requested_nickname", "worker")
					if directPOST(t, h, next, form).Code != http.StatusOK {
						t.Fatal("admin detach did not release identity for fresh owner admission")
					}
				}
			})
		}
	}
}
