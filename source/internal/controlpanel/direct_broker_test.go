package controlpanel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The fixture uses the actual broker and policy callbacks. It supplies only
// the existing admission adapter, not fake persistence or fake proof results.
type directAdmissionFixture struct{}

func (directAdmissionFixture) Admit(ctx context.Context, r broker.RegisterRequest) (string, string, error) {
	g, _ := access.FromContext(ctx)
	return g.Nickname, "", nil
}
func (directAdmissionFixture) RegistrationOwner(ctx context.Context) string {
	g, _ := access.FromContext(ctx)
	return g.OwnerID
}
func (directAdmissionFixture) Claim(ctx context.Context, r broker.RegisterRequest) (string, error) {
	g, _ := access.FromContext(ctx)
	if err := g.Claim(bindingForDirect(r)); err != nil {
		return broker.CodeInvalidToken, nil
	}
	return "", nil
}
func (directAdmissionFixture) CheckSession(ctx context.Context, s broker.Session) error {
	g, ok := access.FromContext(ctx)
	if !ok {
		return nil
	}
	identity := DeviceRequest{Harness: s.Harness, SessionID: s.ID, InstanceID: s.ClientID}.identity()
	if err := g.CheckName(identity, s.Name); err != nil {
		return err
	}
	return g.Check(identity)
}
func bindingForDirect(r broker.RegisterRequest) string {
	return DeviceRequest{Harness: r.Harness, SessionID: r.SessionID, InstanceID: r.ClientID}.identity()
}
func directRegisterRequest() broker.RegisterRequest {
	return broker.RegisterRequest{Holder: g4FixtureHolder(), Harness: "codex", SessionID: "11111111-1111-4111-8111-111111111111", ClientID: "client-direct-test", Name: "worker", PID: 1, CWD: "/synthetic", ChannelID: "direct-lease-a", Secret: strings.Repeat("a", 40), RequestID: "register-direct"}
}
func directGrant(t *testing.T, s *Store, raw string) access.Grant {
	t.Helper()
	g, _, ok := s.authorize("demo", raw)
	if !ok {
		t.Fatal("direct child not authorized")
	}
	return g
}
func confirmDirect(t *testing.T, g access.Grant, out broker.RegisterResult) error {
	t.Helper()
	return g.Registration(DeviceRequest{Harness: out.Session.Harness, SessionID: out.Session.ID, InstanceID: out.Session.ClientID}.identity(), out.Session.RegistrationID, g.OwnerID, out.Session.Name, out.Session.Revision, true)
}
func brokerProof(t *testing.T, remote *broker.RemoteService, r DirectRequest) DirectProof {
	t.Helper()
	p, err := remote.InspectDirectRegistration(t.Context(), r.Harness, r.SessionID, r.InstanceID, r.RegistrationID, r.ResumeSecret)
	if err != nil {
		t.Fatal("valid proof rejected")
	}
	return DirectProof{RegistrationID: p.RegistrationID, OwnerID: p.OwnerID, Name: p.Name, Revision: p.Revision, AdmissionGrantID: p.AdmissionGrantID, Names: p.Names}
}
func TestDirectBrokerConfirmationLossRestartAndProofRecovery(t *testing.T) {
	s := directStore(t)
	parent := directParent(t, s, 1)
	r := directRequest(directForm())
	issued, err := s.ExchangeDirect("demo", parent, r, DirectProof{}, "peer", false)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	remote, err := broker.NewSQLitePersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	remote.SetAdmission(directAdmissionFixture{})
	g := directGrant(t, s, issued.AccessToken)
	req := directRegisterRequest()
	out, err := remote.RegisterSession(access.WithGrant(t.Context(), g), req)
	if err != nil || out.Code != "" {
		t.Fatalf("registration code=%s", out.Code)
	}
	if !remote.RegistrationAdmissionMatches(out.Session.RegistrationID, g.TokenID) {
		t.Fatal("server admission metadata missing")
	}
	if s.data.Projects["demo"].AutoBindings[0].State != "claiming" {
		t.Fatal("broker committed before durable claiming")
	}
	s.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
	if err = confirmDirect(t, g, out); !errors.Is(err, ErrStorage) {
		t.Fatal("confirmation failure not returned")
	}
	dir := s.root.Name()
	s.Close()
	remote.Close()
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.data.ScopeIssuanceReady = true
	remote, err = broker.NewSQLitePersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	remote.SetAdmission(directAdmissionFixture{})
	g = directGrant(t, s, issued.AccessToken)
	replayed, err := remote.RegisterSession(access.WithGrant(t.Context(), g), req)
	if err != nil || replayed.Code != "" || replayed.Session.RegistrationID != out.Session.RegistrationID {
		t.Fatal("cross-store replay lost registration")
	}
	if err = confirmDirect(t, g, replayed); err != nil {
		t.Fatal(err)
	}
	if s.data.Projects["demo"].AutoBindings[0].State != "confirmed" {
		t.Fatal("cross-store replay did not confirm")
	}
	if _, err = remote.InspectDirectRegistration(t.Context(), r.Harness, r.SessionID, r.InstanceID, out.Session.RegistrationID, "wrong-proof"); err == nil {
		t.Fatal("bad proof accepted")
	}
	r.RequestID = "recover"
	r.RegistrationID = out.Session.RegistrationID
	r.ResumeSecret = req.Secret
	proof := brokerProof(t, remote, r)
	recovered, err := s.ExchangeDirect("demo", parent, r, proof, "peer", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.data.Projects["demo"].AutoBindings) != 1 || recovered.RegistrationID != out.Session.RegistrationID {
		t.Fatal("recovery allocated extra slot")
	}
	g = directGrant(t, s, recovered.AccessToken)
	req.ExpectedRevision = recovered.Revision
	req.RequestID = "recover-register"
	req.ChannelID = "direct-lease-b"
	again, err := remote.RegisterSession(access.WithGrant(t.Context(), g), req)
	if err != nil || again.Code != "" || again.Session.RegistrationID != out.Session.RegistrationID {
		t.Fatal("proof recovery did not preserve registration")
	}
	if err = confirmDirect(t, g, again); err != nil {
		t.Fatal(err)
	}
}

type directBrokerBackend struct{ remote *broker.RemoteService }

func (b directBrokerBackend) Sessions(ctx context.Context, _ string) ([]Session, error) {
	sessions, err := b.remote.ListSessions(ctx)
	var out []Session
	owners := b.remote.RegistrationOwners()
	for _, s := range sessions {
		out = append(out, Session{RegistrationID: s.RegistrationID, SessionID: s.ID, Harness: s.Harness, ClientID: s.ClientID, Name: s.Name, OwnerID: owners[s.RegistrationID]})
	}
	return out, err
}
func (b directBrokerBackend) Detach(context.Context, string, string) error {
	return errors.New("unused")
}
func (b directBrokerBackend) InspectDirectRegistration(ctx context.Context, _ string, r DirectRequest) (DirectProof, error) {
	p, err := b.remote.InspectDirectRegistration(ctx, r.Harness, r.SessionID, r.InstanceID, r.RegistrationID, r.ResumeSecret)
	return DirectProof{RegistrationID: p.RegistrationID, OwnerID: p.OwnerID, Name: p.Name, Revision: p.Revision, AdmissionGrantID: p.AdmissionGrantID, Names: p.Names}, err
}
func (b directBrokerBackend) RenameDirectRegistration(ctx context.Context, _ string, r DirectRequest, p DirectProof) (DirectProof, error) {
	v, err := b.remote.RenameDirectRegistration(ctx, r.RegistrationID, r.ResumeSecret, p.Revision, r.RequestedNickname)
	return DirectProof{RegistrationID: v.RegistrationID, OwnerID: v.OwnerID, Name: v.Name, Revision: v.Revision, AdmissionGrantID: v.AdmissionGrantID}, err
}

func TestDirectRecoveryRenameUsesAtomicJournal(t *testing.T) {
	s := directStore(t)
	parent := directParent(t, s, 1)
	r := directRequest(directForm())
	issued, err := s.ExchangeDirect("demo", parent, r, DirectProof{}, "peer", false)
	if err != nil {
		t.Fatal(err)
	}
	remote := broker.NewRemoteService()
	defer remote.Close()
	remote.SetAdmission(directAdmissionFixture{})
	g := directGrant(t, s, issued.AccessToken)
	req := directRegisterRequest()
	out, err := remote.RegisterSession(access.WithGrant(t.Context(), g), req)
	if err != nil || out.Code != "" || confirmDirect(t, g, out) != nil {
		t.Fatal("initial register")
	}
	form := directForm()
	form.Set("request_id", "rename-recovery")
	form.Set("registration_id", out.Session.RegistrationID)
	form.Set("resume_secret", req.Secret)
	form.Set("requested_nickname", "renamed-worker")
	h := New(s, directBrokerBackend{remote}, http.NotFoundHandler())
	w := directPOST(t, h, parent, form)
	if w.Code != 200 {
		t.Fatalf("recovery rename status=%d", w.Code)
	}
	var result DirectResponse
	if json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal("response JSON")
	}
	if result.Nickname != "renamed-worker" || result.Revision != out.Session.Revision+1 {
		t.Fatal("rename response not authoritative")
	}
	g = directGrant(t, s, result.AccessToken)
	req.RequestID = "after-rename"
	req.ChannelID = "rename-lease"
	req.ExpectedRevision = result.Revision
	again, err := remote.RegisterSession(access.WithGrant(t.Context(), g), req)
	if err != nil || again.Code != "" || again.Session.Name != result.Nickname || again.Session.RegistrationID != out.Session.RegistrationID {
		t.Fatal("rename registration fence failed")
	}
	if confirmDirect(t, g, again) != nil {
		t.Fatal("renamed grant confirmation")
	}
}
func TestDirectParentRevocationAndExpiryRetainClaimedCap(t *testing.T) {
	for _, expiry := range []bool{false, true} {
		t.Run(fmt.Sprint(expiry), func(t *testing.T) {
			s := directStore(t)
			now := time.Now().UTC()
			s.now = func() time.Time { return now }
			parent := directParent(t, s, 1)
			issued, err := s.ExchangeDirect("demo", parent, directRequest(directForm()), DirectProof{}, "peer", false)
			if err != nil {
				t.Fatal(err)
			}
			remote := broker.NewRemoteService()
			defer remote.Close()
			remote.SetAdmission(directAdmissionFixture{})
			g := directGrant(t, s, issued.AccessToken)
			out, err := remote.RegisterSession(access.WithGrant(t.Context(), g), directRegisterRequest())
			if err != nil || out.Code != "" || confirmDirect(t, g, out) != nil {
				t.Fatal("register")
			}
			if expiry {
				now = now.Add(31 * 24 * time.Hour)
				if err = s.SweepExpired(); err != nil {
					t.Fatal(err)
				}
			} else {
				p := s.data.Projects["demo"]
				for _, v := range p.Tokens {
					if v.Kind == tokenProject {
						if err = s.RevokeTokenFor("admin", "demo", v.ID); err != nil {
							t.Fatal(err)
						}
						break
					}
				}
			}
			if _, _, ok := s.authorize("demo", issued.AccessToken); ok {
				t.Fatal("inactive parent left child authorized")
			}
			select {
			case <-g.Revoked:
			default:
				t.Fatal("inactive parent left stream watch open")
			}
			if len(s.data.Projects["demo"].AutoBindings) != 1 {
				t.Fatal("retained registration silently freed cap")
			}
			recorded := false
			for _, e := range s.audits {
				if e.GrantID == issued.GrantID && e.Kind == "revoked" && e.Reason == "parent-inactive" {
					recorded = true
				}
			}
			if !recorded {
				t.Fatal("parent inactivity did not audit child revocation")
			}
			if remote.RegistrationOwners()[out.Session.RegistrationID] != "admin" {
				t.Fatal("parent inactivity changed registration")
			}
		})
	}
}

// This adapter represents a separate, authorized manual ownership mutation.
// It deliberately has no direct-grant callback, as the old/manual path does.
type manualOwnerMutation struct{ owner string }

func (m manualOwnerMutation) Admit(_ context.Context, r broker.RegisterRequest) (string, string, error) {
	return r.Name, "", nil
}
func (m manualOwnerMutation) RegistrationOwner(context.Context) string { return m.owner }
func TestDirectProofWrongOwnerAndFinalRevisionFence(t *testing.T) {
	s := directStore(t)
	parent := directParent(t, s, 8)
	r := directRequest(directForm())
	issued, err := s.ExchangeDirect("demo", parent, r, DirectProof{}, "peer", false)
	if err != nil {
		t.Fatal(err)
	}
	remote := broker.NewRemoteService()
	defer remote.Close()
	remote.SetAdmission(directAdmissionFixture{})
	req := directRegisterRequest()
	g := directGrant(t, s, issued.AccessToken)
	out, err := remote.RegisterSession(access.WithGrant(t.Context(), g), req)
	if err != nil || out.Code != "" || confirmDirect(t, g, out) != nil {
		t.Fatal("initial register")
	}
	if s.CreateUser("admin", "other", testPassword, "user") != nil || s.SetProjectMember("admin", "demo", "other", "owner") != nil {
		t.Fatal("other owner fixture")
	}
	other, err := s.IssueProjectTokenWithOptionsFor("other", "demo", "other", ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto)})
	if err != nil {
		t.Fatal(err)
	}
	r.RequestID = "wrong-owner"
	r.RegistrationID = out.Session.RegistrationID
	r.ResumeSecret = req.Secret
	proof := brokerProof(t, remote, r)
	before := len(s.data.Projects["demo"].Tokens)
	if _, err = s.ExchangeDirect("demo", other, r, proof, "peer", false); !errors.Is(err, errDirectConflict) {
		t.Fatal("another parent owner took over registration")
	}
	if len(s.data.Projects["demo"].Tokens) != before {
		t.Fatal("wrong owner created grant")
	}
	r.RequestID = "proof-fence"
	next, err := s.ExchangeDirect("demo", parent, r, proof, "peer", false)
	if err != nil {
		t.Fatal(err)
	}
	// A manual actor changes revision/owner after the proof was observed.
	remote.SetAdmission(manualOwnerMutation{"other"})
	manual := req
	manual.ExpectedRevision = out.Session.Revision
	manual.RequestID = "manual-update"
	manual.ChannelID = "manual-lease"
	changed, err := remote.RegisterSession(t.Context(), manual)
	if err != nil || changed.Code != "" {
		t.Fatal("manual competing change")
	}
	remote.SetAdmission(directAdmissionFixture{})
	g = directGrant(t, s, next.AccessToken)
	req.ExpectedRevision = changed.Session.Revision
	req.RequestID = "stale-proof-register"
	req.ChannelID = "stale-direct-lease"
	rejected, err := remote.RegisterSession(access.WithGrant(t.Context(), g), req)
	if err != nil || rejected.Code == "" {
		t.Fatal("stale direct proof passed final fence")
	}
	if remote.RegistrationOwners()[out.Session.RegistrationID] != "other" {
		t.Fatal("stale grant transferred ownership")
	}
}

func TestDirectRenameFinalPolicyWriteLossRecoversJournal(t *testing.T) {
	s := directStore(t)
	parent := directParent(t, s, 1)
	r := directRequest(directForm())
	issued, err := s.ExchangeDirect("demo", parent, r, DirectProof{}, "peer", false)
	if err != nil {
		t.Fatal(err)
	}
	remote := broker.NewRemoteService()
	defer remote.Close()
	remote.SetAdmission(directAdmissionFixture{})
	req := directRegisterRequest()
	g := directGrant(t, s, issued.AccessToken)
	out, err := remote.RegisterSession(access.WithGrant(t.Context(), g), req)
	if err != nil || out.Code != "" || confirmDirect(t, g, out) != nil {
		t.Fatal("initial register")
	}
	r.RequestID = "rename-journal"
	r.RegistrationID = out.Session.RegistrationID
	r.ResumeSecret = req.Secret
	r.RequestedNickname = "journal-name"
	proof := brokerProof(t, remote, r)
	next, err := s.ExchangeDirect("demo", parent, r, proof, "peer", false)
	if err != nil {
		t.Fatal(err)
	}
	g = directGrant(t, s, next.AccessToken)
	write := s.write
	calls := 0
	s.write = func(data []byte) error {
		calls++
		if calls == 2 {
			return ErrStorage
		}
		return write(data)
	}
	if _, err = remote.RenameDirectRegistration(access.WithGrant(t.Context(), g), r.RegistrationID, r.ResumeSecret, proof.Revision, r.RequestedNickname); err == nil {
		t.Fatal("final policy write failure ignored")
	}
	s.write = write
	updated := brokerProof(t, remote, r)
	if updated.Name != "journal-name" {
		t.Fatal("broker rename did not commit before failure")
	}
	recovered, err := s.ExchangeDirect("demo", parent, r, updated, "peer", false)
	if err != nil || recovered.Nickname != "journal-name" || recovered.Revision != updated.Revision {
		t.Fatal("explicit pending rename journal did not recover")
	}
	if len(s.data.Projects["demo"].AutoBindings) != 1 || len(s.data.Projects["demo"].Tokens) != 3 {
		t.Fatal("rename retry allocated extra grant or slot")
	}
}

func TestDirectExplicitDetachReleaseLossRecoversOnlyAfterAbsence(t *testing.T) {
	s := directStore(t)
	parent := directParent(t, s, 1)
	r := directRequest(directForm())
	issued, err := s.ExchangeDirect("demo", parent, r, DirectProof{}, "peer", false)
	if err != nil {
		t.Fatal(err)
	}
	remote := broker.NewRemoteService()
	defer remote.Close()
	remote.SetAdmission(directAdmissionFixture{})
	g := directGrant(t, s, issued.AccessToken)
	req := directRegisterRequest()
	out, err := remote.RegisterSession(access.WithGrant(t.Context(), g), req)
	if err != nil || out.Code != "" || confirmDirect(t, g, out) != nil {
		t.Fatal("initial register")
	}
	selector := broker.SelectorRegistration + out.Session.RegistrationID
	if err = remote.StopSession(t.Context(), selector); err != nil {
		t.Fatal(err)
	}
	if err = remote.RemoveDetachedSession(t.Context(), selector, func(current broker.Session) error { return s.RevokeSessionTokens("demo", current.Name, r.identity()) }); err != nil {
		t.Fatal(err)
	}
	if !s.data.Projects["demo"].AutoBindings[0].DetachPending {
		t.Fatal("explicit detach intent was not durable")
	}
	write, batch := s.write, s.writeBatch
	s.write = func([]byte) error { return ErrStorage }
	s.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
	if err = s.ReleaseDirectRegistration("demo", r.identity()); !errors.Is(err, ErrStorage) {
		t.Fatal("release write loss not surfaced")
	}
	s.write, s.writeBatch = write, batch
	r.RequestID = "after-release-loss"
	p, err := remote.InspectDirectRegistration(t.Context(), r.Harness, r.SessionID, r.InstanceID, "", "")
	if err != nil || !p.Absent {
		t.Fatal("removed registration not verified absent")
	}
	next, err := s.ExchangeDirect("demo", parent, r, DirectProof{Absent: p.Absent}, "peer", false)
	if err != nil {
		t.Fatal(err)
	}
	if next.GrantID == issued.GrantID || len(s.data.Projects["demo"].AutoBindings) != 1 {
		t.Fatal("release recovery cap not reused exactly once")
	}
}
