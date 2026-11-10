package serverruntime

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestActivityAuthorizationIsolationRevocationAndReapproval(t *testing.T) {
	store := protectedStore(t)
	remote := broker.NewRemoteService()
	defer remote.Close()
	service := newProtectedBackend("project", remote)
	owner, _ := protectedGrant(t, store, "activity-owner")
	other, _ := protectedGrant(t, store, "other-owner")
	req := protectedRequest(1, "ignored")
	ctx := access.WithGrant(t.Context(), owner)
	registered, err := service.RegisterSession(ctx, req)
	if err != nil || registered.Code != "" {
		t.Fatal("grant registration failed")
	}
	sample := protocol.Activity{State: protocol.ActivityWorking, Source: "codex-rollout", Sequence: 1, ObservedAt: time.Now().UTC()}
	raw, _ := json.Marshal(sample)
	renew := broker.RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: req.ChannelID, Activity: raw}
	if out, err := service.RenewLease(ctx, renew); err != nil || out.Code != "" || out.Session.Activity.State != "working" {
		t.Fatal("owner sample denied")
	}
	for name, grant := range map[string]access.Grant{"other binding": other, "other project": func() access.Grant { g := owner; g.Project = "foreign"; return g }()} {
		t.Run(name, func(t *testing.T) {
			denied, err := service.RenewLease(access.WithGrant(t.Context(), grant), renew)
			if err == nil && denied.Code == "" {
				t.Fatal("unauthorized activity accepted")
			}
			current, err := service.GetSession(t.Context(), registered.Session.RegistrationID)
			if err != nil || current.Activity.State != "working" || current.Activity.Sequence != 1 {
				t.Fatal("unauthorized request altered victim activity")
			}
		})
	}
	if err := store.RevokeToken("project", owner.TokenID); err != nil {
		t.Fatal(err)
	}
	// Even an HTTP rejection before RenewLease cannot leave accepted working visible.
	current, err := service.GetSession(t.Context(), registered.Session.RegistrationID)
	if err != nil || current.Activity.State != "unknown" || current.Status != broker.StatusAlive || current.RegistrationID != registered.Session.RegistrationID {
		t.Fatal("invalidated grant kept activity or modified registration")
	}
	if denied, err := service.RenewLease(ctx, renew); err == nil && denied.Code == "" {
		t.Fatal("revoked grant updated activity")
	}
	fresh, _ := protectedGrant(t, store, "activity-owner")
	req.ChannelID = "activity-reapproved-lease"
	req.RequestID = "activity-reapproved-request"
	req.ExpectedRevision = current.Revision
	approved, err := service.RegisterSession(access.WithGrant(t.Context(), fresh), req)
	if err != nil || approved.Code != "" || approved.Session.RegistrationID != registered.Session.RegistrationID || approved.Session.Activity.State != "unknown" {
		t.Fatal("reapproval failed to reset same registration activity")
	}
	sample.Sequence = 1
	sample.ObservedAt = time.Now().UTC()
	raw, _ = json.Marshal(sample)
	renew.ChannelID = req.ChannelID
	renew.Activity = raw
	if out, err := service.RenewLease(access.WithGrant(t.Context(), fresh), renew); err != nil || out.Code != "" || out.Session.Activity.State != "working" {
		t.Fatal("reapproved binding could not apply fresh activity")
	}
}
