package serverruntime

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"testing"
)

func TestManagedRenamePreservesConnectionAndSurvivesRestart(t *testing.T) {
	r, projectToken := newDeviceRuntime(t)
	a, b := protectedRequest(1, "ignored"), protectedRequest(2, "ignored")
	token := r.authorize(projectToken, a, "before")
	peerToken := r.authorize(projectToken, b, "peer")
	original := managedTool[broker.RegisterResult](t, r.bound(token), "register_session", a).Session
	managedTool[broker.RegisterResult](t, r.bound(peerToken), "register_session", b)
	managedTool[broker.SendResult](t, r.bound(peerToken), "send_message", broker.Message{From: "peer", To: "before", Body: "queued"})
	changed := managedTool[broker.Session](t, r.bound(token), "rename_session", map[string]string{"registrationId": original.RegistrationID, "channelId": a.ChannelID, "name": "after"})
	if changed.Name != "after" || changed.ID != original.ID || changed.RegistrationID != original.RegistrationID || changed.Generation != original.Generation || !changed.AutomaticDeliveryReady {
		t.Fatalf("rename lost connection: %+v", changed)
	}
	queued := managedTool[broker.Message](t, r.bound(token), "wait_for_message", map[string]string{"recipient": "after", "channelId": a.ChannelID})
	if queued.Body != "queued" {
		t.Fatalf("queue lost: %+v", queued)
	}
	managedTool[broker.SendResult](t, r.bound(token), "send_message", broker.Message{From: "after", To: "peer", Body: "new sender"})
	received := managedTool[broker.Message](t, r.bound(peerToken), "wait_for_message", map[string]string{"recipient": "peer", "channelId": b.ChannelID})
	if received.From != "after" {
		t.Fatalf("old sender: %+v", received)
	}
	for _, tc := range []struct{ name, credential, lease string }{{"peer", token, a.ChannelID}, {"bad\nname", token, a.ChannelID}, {"stolen", peerToken, a.ChannelID}, {"bad-lease", token, "wrong"}} {
		var result struct{ IsError bool }
		managedRPC(t, r.bound(tc.credential), "tools/call", map[string]any{"name": "rename_session", "arguments": map[string]string{"registrationId": original.RegistrationID, "channelId": tc.lease, "name": tc.name}}, &result)
		if !result.IsError {
			t.Fatalf("accepted invalid rename %q", tc.name)
		}
	}
	same := managedTool[broker.Session](t, r.bound(token), "rename_session", map[string]string{"registrationId": original.RegistrationID, "channelId": a.ChannelID, "name": "after"})
	if same.Revision != changed.Revision {
		t.Fatal("same-name retry changed revision")
	}
	r.restart()
	resumed := managedTool[broker.RegisterResult](t, r.bound(token), "resume_session", broker.ResumeRequest{Holder: a.Holder, RegistrationID: original.RegistrationID, Harness: a.Harness, SessionID: a.SessionID, PID: a.PID, CWD: a.CWD, ClientID: a.ClientID, ChannelID: "resumed", Secret: a.Secret, RequestID: "resume-rename", ExpectedRevision: changed.Revision})
	if resumed.Code != "" || resumed.Session.Name != "after" || !resumed.Session.AutomaticDeliveryReady {
		t.Fatalf("renamed credential cannot resume: %+v", resumed)
	}
	a.ChannelID, a.RequestID, a.ExpectedRevision = "resumed", "explicit-after-rename", resumed.Session.Revision
	attached := managedTool[broker.RegisterResult](t, r.bound(token), "register_session", a)
	if attached.Code != "" || attached.Session.Name != "after" {
		t.Fatalf("explicit attach reverted rename: %+v", attached)
	}
}
