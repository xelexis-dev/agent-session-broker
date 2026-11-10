package controlpanel

import (
	"fmt"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func detachedCleanupBatchFixture(t *testing.T) (*cleanupFixture, []access.DetachedRemovalProof) {
	t.Helper()
	f := detachedCleanupFixture(t)
	f.detach(t)
	proofs := []access.DetachedRemovalProof{f.proof}
	for i := 1; i < 3; i++ {
		form := directForm()
		form.Set("session_id", fmt.Sprintf("11111111-1111-4111-8111-%012d", i+9000))
		form.Set("asb_client_instance_id", fmt.Sprintf("batch-client-%d", i))
		form.Set("request_id", fmt.Sprintf("batch-request-%d", i))
		issued, err := f.store.ExchangeDirect("demo", f.parent, directRequest(form), DirectProof{Absent: true}, "fixture", false)
		if err != nil {
			t.Fatal("batch issuance")
		}
		g := directGrant(t, f.store, issued.AccessToken)
		req := directRegisterRequest()
		req.SessionID = form.Get("session_id")
		req.ClientID = form.Get("asb_client_instance_id")
		req.RequestID = form.Get("request_id")
		req.ChannelID = fmt.Sprintf("batch-lease-%d", i)
		req.Capabilities = []string{"registration.reattach-after-cleanup"}
		out, err := f.remote.RegisterSession(access.WithGrant(t.Context(), g), req)
		if err != nil || out.Code != "" {
			t.Fatal("batch registration")
		}
		if err := confirmDirect(t, g, out); err != nil {
			t.Fatal("batch confirmation")
		}
		if err := f.remote.StopSession(access.WithGrant(t.Context(), g), out.Session.RegistrationID); err != nil {
			t.Fatal("batch detach")
		}
		proofs = append(proofs, access.DetachedRemovalProof{RegistrationID: out.Session.RegistrationID, Revision: out.Session.Revision, Identity: bindingForDirect(req), OwnerID: g.OwnerID, Name: out.Session.Name, AdmissionMode: g.AdmissionMode, GrantID: g.TokenID, ParentID: g.ParentID, Detached: true, ReattachAfterCleanup: true})
	}

	return f, proofs
}

func TestDetachedCleanupBatchOnePolicyCommit(t *testing.T) {
	f, proofs := detachedCleanupBatchFixture(t)
	writes := 0
	original := f.store.writeBatch
	f.store.writeBatch = func(puts map[string][]byte, deletes []string) error { writes++; return original(puts, deletes) }
	generation := f.store.ProjectGeneration("demo")
	if err := f.store.CleanupDetachedAutoBatch("demo", generation, proofs); err != nil {
		t.Fatal("batch policy failed")
	}
	if writes != 1 {
		t.Fatalf("detached rows produced %d policy commits, want 1", writes)
	}
	if len(f.store.data.Projects["demo"].AutoBindings) != 0 {
		t.Fatal("batch left confirmed bindings")
	}
}

func TestDetachedCleanupBatchAtomicFailureAndDuplicateProofs(t *testing.T) {
	f, proofs := detachedCleanupBatchFixture(t)
	generation := f.store.ProjectGeneration("demo")
	original := f.store.writeBatch
	before := len(f.store.audits)
	f.store.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
	if err := f.store.CleanupDetachedAutoBatch("demo", generation, proofs); err == nil {
		t.Fatal("policy failure ignored")
	}
	if f.revoked() || len(f.store.data.Projects["demo"].AutoBindings) != 3 || len(f.store.audits) != before {
		t.Fatal("failed batch changed policy or audit")
	}
	f.store.writeBatch = original
	// Duplicated proofs must see the earlier revocation instead of generating new audit.
	doubled := append(append([]access.DetachedRemovalProof{}, proofs...), proofs...)
	if err := f.store.CleanupDetachedAutoBatch("demo", generation, doubled); err != nil {
		t.Fatal("retry failed")
	}
	if !f.revoked() || len(f.store.data.Projects["demo"].AutoBindings) != 0 || len(f.store.audits) != before+3 {
		t.Fatal("retry did not converge exactly once per grant")
	}
	if err := f.store.CleanupDetachedAutoBatch("demo", generation, proofs); err != nil || len(f.store.audits) != before+3 {
		t.Fatal("replay changed audit")
	}
}

func TestReviewDetachedCleanupNotifiesPanelChange(t *testing.T) {
	f, proofs := detachedCleanupBatchFixture(t)
	watcher, err := f.store.IssueToken("demo", "panel-watcher")
	if err != nil {
		t.Fatal("panel watcher fixture")
	}
	_, changed, ok := f.store.authorize("demo", watcher)
	if !ok || changed == nil {
		t.Fatal("panel change subscription")
	}
	original := f.store.writeBatch
	f.store.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
	generation := f.store.ProjectGeneration("demo")
	if f.store.CleanupDetachedAutoBatch("demo", generation, proofs) == nil {
		t.Fatal("failure injection")
	}
	select {
	case <-changed:
		t.Fatal("failed commit notified panel")
	default:
	}
	f.store.writeBatch = original
	if f.store.CleanupDetachedAutoBatch("demo", generation, proofs) != nil {
		t.Fatal("cleanup failed")
	}
	select {
	case <-changed:
	default:
		t.Fatal("F1 revocation did not notify panel subscriber")
	}
	_, next, ok := f.store.authorize("demo", watcher)
	if !ok {
		t.Fatal("independent watcher must survive")
	}
	if f.store.CleanupDetachedAutoBatch("demo", generation, proofs) != nil {
		t.Fatal("idempotent replay")
	}
	select {
	case <-next:
		t.Fatal("no-op cleanup notified panel")
	default:
	}
}
