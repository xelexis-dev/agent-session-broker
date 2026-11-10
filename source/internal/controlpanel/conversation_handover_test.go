package controlpanel

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestConversationHandoverPolicyOwnerMismatch(t *testing.T) {
	s := directStore(t)
	if s.CreateUser("admin", "other", testPassword, "user") != nil || s.SetProjectMember("admin", "demo", "other", "owner") != nil {
		t.Fatal("owner setup")
	}
	parent, err := s.IssueProjectTokenWithOptionsFor("other", "demo", "other", ProjectTokenOptions{Scopes: append(access.LegacyV1Scopes(), access.SessionRegisterAuto)})
	if err != nil {
		t.Fatal(err)
	}
	r := directRequest(directForm())
	r.Harness = "claude"
	r.Handover = true
	r.RegistrationID = "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	r.ResumeSecret = "synthetic"
	_, err = s.ExchangeDirect("demo", parent, r, DirectProof{RegistrationID: r.RegistrationID, PreviousIdentity: r.identity(), OwnerID: "admin", Name: "worker", Revision: 1}, "peer", false)
	var refusal protocol.HandoverRefusal
	if !errors.As(err, &refusal) || refusal.Reason != "owner-mismatch" || len(s.data.Projects["demo"].AutoBindings) != 0 {
		t.Fatal("owner mismatch issued a handover credential")
	}
}

type refuseHandoverBackend struct {
	tokenViewBackend
	proof DirectProof
}

func (b refuseHandoverBackend) InspectDirectRegistration(_ context.Context, _ string, r DirectRequest) (DirectProof, error) {
	if r.Handover {
		return b.proof, nil
	}
	return DirectProof{Absent: true}, nil
}
func (b refuseHandoverBackend) RebindConversation(context.Context, string, DirectRequest, DirectProof) (DirectProof, error) {
	return DirectProof{}, protocol.HandoverRefusal{Reason: "still-receiving"}
}
func TestConversationHandoverDefinitiveRefusalReleasesTemporaryChild(t *testing.T) {
	s := directStore(t)
	parent := directParent(t, s, 8)
	old := directRequest(directForm())
	old.Harness = "claude"
	child, err := s.ExchangeDirect("demo", parent, old, DirectProof{Absent: true}, "peer", false)
	if err != nil {
		t.Fatal(err)
	}
	id := "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	if err = s.directRegistration("demo", child.GrantID, old.identity(), id, "", "worker", 0, false); err != nil {
		t.Fatal(err)
	}
	if err = s.directRegistration("demo", child.GrantID, old.identity(), id, "admin", "worker", 1, true); err != nil {
		t.Fatal(err)
	}
	backend := refuseHandoverBackend{proof: DirectProof{RegistrationID: id, OwnerID: "admin", Name: "worker", Revision: 1, PreviousIdentity: old.identity(), AdmissionGrantID: child.GrantID}}
	h := New(s, backend, http.NotFoundHandler())
	form := directForm()
	form.Set("harness", "claude")
	form.Set("session_id", "22222222-2222-4222-8222-222222222222")
	form.Set("request_id", "switch")
	form.Set("requested_nickname", "")
	form.Set("handover", "true")
	form.Set("registration_id", id)
	form.Set("resume_secret", "synthetic")
	form.Set("pid", "123")
	form.Set("cwd", "/synthetic")
	if w := directPOST(t, h, parent, form); w.Code != 409 {
		t.Fatal("broker refusal was not returned")
	}
	if _, _, ok := s.authorize("demo", child.AccessToken); !ok {
		t.Fatal("rejected handover revoked original credential")
	}
	if len(s.data.Projects["demo"].AutoBindings) != 1 {
		t.Fatal("rejected handover retained its temporary binding")
	}
	// Ordinary fresh admission for that new identity must remain possible.
	for _, key := range []string{"handover", "registration_id", "resume_secret", "pid", "cwd"} {
		form.Del(key)
	}
	form.Set("request_id", "fresh-after-refusal")
	if w := directPOST(t, h, parent, form); w.Code != 200 {
		t.Fatal("refused automatic candidate blocked fresh fallback")
	}
}
func TestConversationHandoverCannotRenameDuringExchange(t *testing.T) {
	s := directStore(t)
	parent := directParent(t, s, 8)
	old := directRequest(directForm())
	old.Harness = "claude"
	h := New(s, refuseHandoverBackend{proof: DirectProof{RegistrationID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", OwnerID: "admin", Name: "kept", Revision: 1, PreviousIdentity: old.identity()}}, http.NotFoundHandler())
	form := url.Values{"client_id": {"asb-client-v1"}, "harness": {"claude"}, "session_id": {"22222222-2222-4222-8222-222222222222"}, "asb_client_instance_id": {"client-direct-test"}, "request_id": {"rename-attempt"}, "handover": {"true"}, "registration_id": {"AAAAAAAAAAAAAAAAAAAAAAAAAA"}, "resume_secret": {"synthetic"}, "requested_nickname": {"changed"}, "pid": {"123"}, "cwd": {"/synthetic"}}
	if w := directPOST(t, h, parent, form); w.Code != 409 || len(s.data.Projects["demo"].AutoBindings) != 0 || len(s.data.Projects["demo"].Tokens) != 1 {
		t.Fatal("handover issued credentials for a rename")
	}
}
