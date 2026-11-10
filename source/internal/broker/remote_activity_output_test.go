package broker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestActivityOutputCapabilityBelongsToCurrentCallerLease(t *testing.T) {
	s, now, req, registered := newActivityFixture(t)
	identity, _ := json.Marshal([3]string{req.Harness, req.SessionID, req.ClientID})
	revoked := false
	caller := access.WithGrant(t.Context(), access.Grant{Check: func(id string) error {
		if revoked || id != string(identity) {
			return errors.New("not authorized")
		}
		return nil
	}})
	if s.SessionActivityOutputAllowed(caller) {
		t.Fatal("no sample enabled activity output")
	}
	// Invalid samples and wrong leases/generations cannot opt a client in.
	activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, map[string]any{"state": "invalid"})
	wrongGeneration := activitySample(*now, 1, "unknown")
	wrongGeneration["generation"] = registered.Session.Generation + 1
	activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, wrongGeneration)
	wrongLease := func(lease string, seq uint64) {
		raw, _ := json.Marshal(activitySample(*now, seq, "unknown"))
		out, err := s.RenewLease(t.Context(), RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: lease, Activity: raw})
		if err != nil || out.Code != CodeInvalid {
			t.Fatal("wrong lease was not rejected")
		}
	}
	wrongLease("wrong-lease", 1)
	if s.SessionActivityOutputAllowed(caller) {
		t.Fatal("invalid evidence enabled activity output")
	}
	sample := activitySample(*now, 1, "unknown")
	sample["source"], sample["reason"] = "", "unsupported"
	activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, sample)
	if !s.SessionActivityOutputAllowed(caller) {
		t.Fatal("valid unknown sample did not enable output")
	}
	if s.SessionActivityOutputAllowed(context.Background()) || s.SessionActivityOutputAllowed(access.WithGrant(t.Context(), access.Grant{})) {
		t.Fatal("unbound caller enabled output")
	}
	other := access.WithGrant(t.Context(), access.Grant{Check: func(string) error { return errors.New("other caller") }})
	if s.SessionActivityOutputAllowed(other) {
		t.Fatal("another registration enabled output")
	}
	*now = now.Add(activityTTL + time.Second)
	got, _ := s.GetSession(t.Context(), req.Name)
	if got.Activity.State != protocol.ActivityUnknown || !s.SessionActivityOutputAllowed(caller) {
		t.Fatal("TTL confused evidence freshness with output capability")
	}
	activityRenewJSON(t, s, registered.Session.RegistrationID, req.ChannelID, nil)
	if !s.SessionActivityOutputAllowed(caller) {
		t.Fatal("missing sample downgraded current lease")
	}
	revoked = true
	if s.SessionActivityOutputAllowed(caller) {
		t.Fatal("revoked grant enabled output")
	}
	revoked = false
	resumed, err := s.ResumeSession(t.Context(), resumeFrom(req, registered.Session.RegistrationID, "next-lease", "next-request", registered.Session.Revision))
	if err != nil || resumed.Code != "" {
		t.Fatal("resume failed")
	}
	if s.SessionActivityOutputAllowed(caller) {
		t.Fatal("replacement lease inherited capability")
	}
	wrongLease(req.ChannelID, 2)
	if s.SessionActivityOutputAllowed(caller) {
		t.Fatal("old lease enabled replacement")
	}
	activityRenewJSON(t, s, registered.Session.RegistrationID, "next-lease", activitySample(*now, 1, "unknown"))
	if !s.SessionActivityOutputAllowed(caller) {
		t.Fatal("new lease could not negotiate output")
	}
}
