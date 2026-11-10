package broker

import (
	"context"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLiveLeaseRejectsCopiedProofOtherReceiverHolder(t *testing.T) {
	s, _ := clockedService(t, "")
	r := durableRegister(801, "holder-owner", "holder-client", durableSecretA, "holder-a", "register-a")
	withHolder := func(v any, pid int, out any) {
		raw, _ := json.Marshal(v)
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		obj["holder"] = map[string]any{"pid": pid, "deviceId": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "incarnation": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
		raw, _ = json.Marshal(obj)
		if json.Unmarshal(raw, out) != nil {
			t.Fatal("holder fixture")
		}
	}
	withHolder(r, 101, &r)
	first, err := s.RegisterSession(context.Background(), r)
	if err != nil || first.Code != "" {
		t.Fatal("fixture register")
	}
	next := resumeFrom(r, first.Session.RegistrationID, "holder-b", "resume-b", first.Session.Revision)
	withHolder(next, 102, &next)
	got, err := s.ResumeSession(context.Background(), next)
	if err != nil || got.Code != "registration-in-use" {
		t.Fatal("copied proof replaced a live receiving process")
	}
	renewed, err := s.RenewLease(context.Background(), RenewRequest{RegistrationID: first.Session.RegistrationID, ChannelID: r.ChannelID})
	if err != nil || renewed.Code != "" {
		t.Fatal("challenger fenced original holder")
	}
}

func syntheticReceiverHolder(pid int, device string) *protocol.ReceiverHolder {
	return &protocol.ReceiverHolder{PID: pid, DeviceID: strings.Repeat(device, 64), Incarnation: strings.Repeat("b", 64)}
}
func TestHolderChallengeFencesExactLeaseAndRevision(t *testing.T) {
	s, now := clockedService(t, "")
	r := durableRegister(802, "holder-challenge", "holder-client", durableSecretA, "lease-a", "register-a")
	r.Holder = syntheticReceiverHolder(201, "a")
	first, _ := s.RegisterSession(t.Context(), r)
	req := resumeFrom(r, first.Session.RegistrationID, "lease-b", "resume-b", 1)
	req.Holder = syntheticReceiverHolder(202, "a")
	blocked, _ := s.ResumeSession(t.Context(), req)
	if blocked.Code != protocol.CodeRegistrationInUse || blocked.HolderChallenge == nil {
		t.Fatal("missing authorized challenge")
	}
	req.HolderProof = &protocol.HolderProof{Challenge: *blocked.HolderChallenge, Dead: true}
	req.HolderProof.Challenge.Generation++
	stale, _ := s.ResumeSession(t.Context(), req)
	if stale.Code != protocol.CodeRegistrationInUse {
		t.Fatal("stale challenge accepted")
	}
	req.HolderProof.Challenge = *blocked.HolderChallenge
	next, err := s.ResumeSession(t.Context(), req)
	if err != nil || next.Code != "" || next.Session.RegistrationID != first.Session.RegistrationID {
		t.Fatal("dead same-device takeover failed")
	}
	replay, _ := s.ResumeSession(t.Context(), req)
	if replay.Code != "" || replay.Session.Revision != next.Session.Revision {
		t.Fatal("exact retry not idempotent")
	}
	late, _ := s.RenewLease(t.Context(), RenewRequest{RegistrationID: first.Session.RegistrationID, ChannelID: r.ChannelID})
	if late.Code == "" {
		t.Fatal("old holder renewed")
	}
	old := req
	old.Holder = r.Holder
	old.ChannelID = "late-a"
	old.RequestID = "late-request"
	old.ExpectedRevision = next.Session.Revision
	old.HolderProof = req.HolderProof
	denied, _ := s.ResumeSession(t.Context(), old)
	if denied.Code != protocol.CodeRegistrationInUse {
		t.Fatal("old challenge stole new lease")
	}
	*now = now.Add(31 * time.Second)
	old.HolderProof = nil
	expired, _ := s.ResumeSession(t.Context(), old)
	if expired.Code != "" {
		t.Fatal("lease expiry did not permit takeover")
	}
}
func TestLegacyAndDifferentDeviceNeverUseDeathException(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		t.Run(map[bool]string{true: "legacy", false: "different-device"}[legacy], func(t *testing.T) {
			s, now := clockedService(t, "")
			r := durableRegister(803, "holder-legacy", "holder-client", durableSecretA, "lease-a", "register-a")
			r.Holder = nil
			if !legacy {
				r.Holder = syntheticReceiverHolder(301, "a")
			}
			first, _ := s.RegisterSession(t.Context(), r)
			req := resumeFrom(r, first.Session.RegistrationID, "lease-b", "resume-b", 1)
			req.Holder = syntheticReceiverHolder(302, "c")
			for i := 0; i < 5; i++ {
				got, _ := s.ResumeSession(t.Context(), req)
				if got.Code != protocol.CodeRegistrationInUse {
					t.Fatal("live lease replaced")
				}
				if legacy && got.HolderChallenge != nil {
					t.Fatal("legacy challenge exposed")
				}
				if got.HolderChallenge != nil {
					req.HolderProof = &protocol.HolderProof{Challenge: *got.HolderChallenge, Dead: true}
				}
				*now = now.Add(20 * time.Second)
				renewed, _ := s.RenewLease(t.Context(), RenewRequest{RegistrationID: first.Session.RegistrationID, ChannelID: r.ChannelID})
				if renewed.Code != "" {
					t.Fatal("holder lost lease")
				}
			}
		})
	}
}
func TestSimultaneousResumeAfterRestartHasOneHolder(t *testing.T) {
	root := t.TempDir()
	s, _ := clockedService(t, root)
	r := durableRegister(804, "holder-restart", "holder-client", durableSecretA, "lease-a", "register-a")
	r.Holder = syntheticReceiverHolder(401, "a")
	first, _ := s.RegisterSession(t.Context(), r)
	s.Close()
	restarted, _ := clockedService(t, root)
	var wg sync.WaitGroup
	codes := make(chan string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := resumeFrom(r, first.Session.RegistrationID, "lease-"+string(rune('b'+i)), "resume-"+string(rune('b'+i)), 1)
			req.Holder = syntheticReceiverHolder(402+i, "a")
			result, err := restarted.ResumeSession(t.Context(), req)
			if err != nil {
				codes <- "error"
			} else {
				codes <- result.Code
			}
		}(i)
	}
	wg.Wait()
	close(codes)
	wins, blocked := 0, 0
	for c := range codes {
		if c == "" {
			wins++
		}
		if c == protocol.CodeRegistrationInUse {
			blocked++
		}
	}
	if wins != 1 || blocked != 1 {
		t.Fatal("restart allowed competing holders")
	}
}
func TestHolderNotDisclosedWithoutProofOrInRoster(t *testing.T) {
	s, _ := clockedService(t, "")
	r := durableRegister(805, "holder-private", "holder-client", durableSecretA, "lease-a", "register-a")
	r.Holder = syntheticReceiverHolder(501, "a")
	first, _ := s.RegisterSession(t.Context(), r)
	req := resumeFrom(r, first.Session.RegistrationID, "lease-b", "resume-b", 1)
	req.Holder = syntheticReceiverHolder(502, "a")
	req.Secret = durableSecretB
	got, _ := s.ResumeSession(t.Context(), req)
	if got.HolderChallenge != nil || got.Code != CodeOwned {
		t.Fatal("unauthorized holder metadata")
	}
	raw, _ := json.Marshal(first.Session)
	if strings.Contains(string(raw), "incarnation") || strings.Contains(string(raw), r.Holder.DeviceID) {
		t.Fatal("holder leaked into session")
	}
}
