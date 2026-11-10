package controlpanel

import (
	"errors"
	"strings"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestManualRebindPolicyConfirmationIsAtomicAndRejectsConcurrentJournals(t *testing.T) {
	f := newDeviceFixture(t)
	old, oldRequest := f.approve(t, "owner", "11111111-1111-4111-8111-111111111111", "original")
	next, _ := f.approve(t, "owner", "22222222-2222-4222-8222-222222222222", "original-next")
	another, _ := f.approve(t, "owner", "33333333-3333-4333-8333-333333333333", "another-next")
	j := protocol.RebindJournal{RequestID: "manual-rebind", TokenID: next.GrantID, SourceDigest: digest(oldRequest.identity()), RegistrationID: strings.Repeat("A", 26), ExpectedRevision: 7, TemporaryNickname: "original-next", Name: "original"}
	if err := f.store.manualRebind("demo", next.GrantID, "pending", j, protocol.RebindResult{}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.manualRebind("demo", next.GrantID, "pending", j, protocol.RebindResult{}); err != nil {
		t.Fatal("same request was not idempotent")
	}
	for _, caller := range []string{next.GrantID, another.GrantID} {
		copy := j
		copy.TokenID = caller
		copy.RequestID = "concurrent"
		if err := f.store.manualRebind("demo", caller, "pending", copy, protocol.RebindResult{}); err == nil {
			t.Fatal("concurrent journal accepted")
		}
	}
	result := protocol.RebindResult{RegistrationID: j.RegistrationID, Name: j.Name, Revision: 8, HandoverFrom: &protocol.HandoverFrom{Revision: 7, At: f.now}}
	f.store.mu.Lock()
	write := f.store.writeBatch
	f.store.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
	f.store.mu.Unlock()
	if err := f.store.manualRebind("demo", next.GrantID, "confirm", j, result); !errors.Is(err, ErrStorage) {
		t.Fatal("injected confirmation commit failure lost")
	}
	f.store.mu.Lock()
	f.store.writeBatch = write
	p := f.store.data.Projects["demo"]
	oldRevoked, callerName := false, ""
	for _, token := range p.Tokens {
		if token.ID == old.GrantID {
			oldRevoked = token.Revoked
		}
		if token.ID == next.GrantID {
			callerName = token.Nickname
		}
	}
	pending := len(p.Rebinds)
	f.store.mu.Unlock()
	if oldRevoked || callerName != "original-next" || pending != 1 {
		t.Fatal("failed policy commit published partial revocation or rename")
	}
	if err := f.store.manualRebind("demo", next.GrantID, "confirm", j, result); err != nil {
		t.Fatal(err)
	}
	if f.store.Authorize("demo", old.AccessToken) {
		t.Fatal("original credential survived confirmation")
	}
	f.store.mu.RLock()
	p = f.store.data.Projects["demo"]
	callerName = ""
	for _, token := range p.Tokens {
		if token.ID == next.GrantID {
			callerName = token.Nickname
		}
	}
	pending = len(p.Rebinds)
	f.store.mu.RUnlock()
	if callerName != "original" || pending != 0 {
		t.Fatal("caller name/journal not confirmed")
	}
	if err := f.store.manualRebind("demo", next.GrantID, "confirm", j, result); err != nil {
		t.Fatal("confirmed replay was not idempotent")
	}
	// This fixture intentionally keeps the audit UI feature gate disabled.
	// Assert the committed event itself; runtime tests cover the enabled API.
	count := 0
	f.store.mu.RLock()
	for _, e := range f.store.audits {
		if e.Project == "demo" && e.Kind == "handover" {
			count++
		}
	}
	f.store.mu.RUnlock()
	if count != 1 {
		t.Fatal("confirmation audit duplicated or missing")
	}
}
func TestManualRebindJournalRejectsRawIdentity(t *testing.T) {
	f := newDeviceFixture(t)
	next, request := f.approve(t, "owner", "22222222-2222-4222-8222-222222222222", "temporary")
	j := protocol.RebindJournal{RequestID: "manual-rebind", TokenID: next.GrantID, SourceDigest: request.identity(), RegistrationID: strings.Repeat("A", 26), ExpectedRevision: 1, TemporaryNickname: "temporary", Name: "original"}
	if err := f.store.manualRebind("demo", next.GrantID, "pending", j, protocol.RebindResult{}); err == nil {
		t.Fatal("raw identity accepted into durable journal")
	}
	if len(f.store.PendingRebinds()) != 0 {
		t.Fatal("invalid journal persisted")
	}
}
