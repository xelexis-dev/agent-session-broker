package serverruntime

import (
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

func TestProtectedRemovedRegistrationReturnsTerminalRecoveryResults(t *testing.T) {
	store := protectedStore(t)
	grant, _ := protectedGrant(t, store, "returning")
	service := newProtectedBackend("project", broker.NewRemoteService())
	defer service.Close()
	ctx := access.WithGrant(t.Context(), grant)
	request := protectedRequest(1, "ignored")
	registered, err := service.RegisterSession(ctx, request)
	if err != nil || registered.Code != "" {
		t.Fatal(registered, err)
	}
	id := registered.Session.RegistrationID
	if err := service.StopSession(ctx, broker.SelectorRegistration+id); err != nil {
		t.Fatal(err)
	}
	if err := service.RemoveDetachedSession(ctx, broker.SelectorRegistration+id, func(broker.Session) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// The credential remains valid while its previous registration is gone.
	// A tool error here makes automatic recovery retry forever under its lock.
	for name, operation := range map[string]func() (string, error){
		"resume": func() (string, error) {
			result, err := service.ResumeSession(ctx, broker.ResumeRequest{RegistrationID: id, Harness: request.Harness, SessionID: request.SessionID, ClientID: request.ClientID, PID: request.PID, CWD: request.CWD, ChannelID: "returning-lease", Secret: request.Secret, RequestID: "returning-request", ExpectedRevision: registered.Session.Revision})
			return result.Code, err
		},
		"renew": func() (string, error) {
			result, err := service.RenewLease(ctx, broker.RenewRequest{RegistrationID: id, ChannelID: request.ChannelID})
			return result.Code, err
		},
		"probe": func() (string, error) {
			result, err := service.AnswerProbe(ctx, broker.ProbeAnswer{RegistrationID: id, ChannelID: request.ChannelID, ProbeID: "expired-probe"})
			return result.Code, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			code, err := operation()
			if err != nil || code != broker.CodeInvalidToken {
				t.Fatalf("recovery must terminate without a tool error: code=%q err=%v", code, err)
			}
		})
	}
	if sessions, err := service.ListSessions(ctx); err != nil || len(sessions) != 0 {
		t.Fatal("recovery resurrected removed registration", sessions, err)
	}
	request.RequestID = "explicit-reregister"
	result, err := service.RegisterSession(ctx, request)
	if err != nil || result.Code != "" || result.Session.RegistrationID == id {
		t.Fatal("explicit registration must remain usable", result, err)
	}
}
