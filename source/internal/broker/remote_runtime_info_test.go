package broker

import (
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"strings"
	"testing"
	"time"
)

func TestRuntimeInfoLeaseSequenceTTLAndInvalidInput(t *testing.T) {
	s, now, req, registered := newActivityFixture(t)
	renew := func(raw []byte, lease string) {
		t.Helper()
		out, err := s.RenewLease(t.Context(), RenewRequest{RegistrationID: registered.Session.RegistrationID, ChannelID: lease, RuntimeInfo: raw, IngressPending: true})
		if err != nil || out.Code != "" {
			t.Fatal("renew rejected")
		}
	}
	sample := func(seq, gen uint64, model string) []byte {
		b, _ := json.Marshal(protocol.RuntimeInfo{Model: model, Effort: "medium", Sequence: seq, Generation: gen, ObservedAt: *now})
		return b
	}
	gen := registered.Session.Generation
	id := registered.Session.RegistrationID
	renew(sample(2, gen, "fixture-model"), req.ChannelID)
	if s.SessionRuntimeInfo()[id].Model != "fixture-model" {
		t.Fatal("pending ingress lost metadata")
	}
	for _, seq := range []uint64{1, 2} {
		renew(sample(seq, gen, "reordered"), req.ChannelID)
	}
	renew(sample(3, gen+1, "wrong-generation"), req.ChannelID)
	if s.SessionRuntimeInfo()[id].Model != "fixture-model" {
		t.Fatal("reordered or wrong generation applied")
	}
	*now = now.Add(25 * time.Second)
	if len(s.SessionRuntimeInfo()) != 0 {
		t.Fatal("expired metadata retained")
	}
	current, _ := s.GetSession(t.Context(), id)
	if current.RegistrationID != id || current.Status != StatusAlive {
		t.Fatal("TTL changed registration")
	}
	for _, raw := range [][]byte{sample(3, gen, strings.Repeat("m", 97)), []byte(`{"model":"unsafe<value>"}`), []byte(`null`), []byte(`{"unknown":true}`), nil} {
		renew(raw, req.ChannelID)
		if len(s.SessionRuntimeInfo()) != 0 {
			t.Fatal("invalid metadata visible")
		}
	}
	renew(sample(4, gen, "fresh-model"), req.ChannelID)
	out, err := s.RenewLease(t.Context(), RenewRequest{RegistrationID: id, ChannelID: "foreign-lease", RuntimeInfo: sample(5, gen, "foreign")})
	if err == nil && out.Code == "" {
		t.Fatal("foreign lease accepted")
	}
	if s.SessionRuntimeInfo()[id].Model != "fresh-model" {
		t.Fatal("foreign lease changed snapshot")
	}
	req.ChannelID = "replacement-lease"
	req.RequestID = "replacement-request"
	req.ExpectedRevision = current.Revision
	replaced, err := s.RegisterSession(t.Context(), req)
	if err != nil || replaced.Code != "" {
		t.Fatal("replacement failed")
	}
	if len(s.SessionRuntimeInfo()) != 0 {
		t.Fatal("new lease inherited metadata")
	}
}

func TestRuntimeInfoDoesNotPersistAcrossSQLiteRestart(t *testing.T) {
	root := t.TempDir()
	s, err := NewSQLitePersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	req := durableRegister(1, "runtime-persist", "client-runtime", durableSecretA, "lease-runtime", "request-runtime")
	out, err := s.RegisterSession(t.Context(), req)
	if err != nil || out.Code != "" {
		t.Fatal("register failed")
	}
	raw, _ := json.Marshal(protocol.RuntimeInfo{Model: "fixture-model", Effort: "high", Sequence: 1, Generation: out.Session.Generation, ObservedAt: time.Now().UTC()})
	if renewed, err := s.RenewLease(t.Context(), RenewRequest{RegistrationID: out.Session.RegistrationID, ChannelID: req.ChannelID, RuntimeInfo: raw}); err != nil || renewed.Code != "" {
		t.Fatal("renew failed")
	}
	if len(s.SessionRuntimeInfo()) != 1 {
		t.Fatal("runtime absent before restart")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLitePersistentRemoteService(root)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if len(restarted.SessionRuntimeInfo()) != 0 {
		t.Fatal("runtime info persisted")
	}
	rows, err := restarted.ListSessions(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatal("restart lost registration")
	}
}
