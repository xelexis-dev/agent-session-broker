package controlpanel

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type cleanupFixture struct {
	parent  string
	store   *Store
	remote  *broker.RemoteService
	request broker.RegisterRequest
	result  broker.RegisterResult
	grant   access.Grant
	proof   access.DetachedRemovalProof
	root    string
}

func detachedCleanupFixture(t *testing.T, harness ...string) *cleanupFixture {
	t.Helper()
	s := directStore(t)
	parent := directParent(t, s, 8)
	form := directForm()
	if len(harness) > 0 {
		form.Set("harness", harness[0])
	}
	issued, err := s.ExchangeDirect("demo", parent, directRequest(form), DirectProof{Absent: true}, "synthetic", false)
	if err != nil {
		t.Fatal("auto issuance", err)
	}
	g := directGrant(t, s, issued.AccessToken)
	f := &cleanupFixture{store: s, grant: g, root: t.TempDir(), request: directRegisterRequest(), parent: parent}
	if len(harness) > 0 {
		f.request.Harness = harness[0]
	}
	generation := s.ProjectGeneration("demo")
	f.remote, err = broker.NewSQLitePersistentRemoteServiceWithBatchCleanup(f.root, func(proofs []access.DetachedRemovalProof) error {
		return s.CleanupDetachedAutoBatch("demo", generation, proofs)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.remote.Close() })
	f.remote.SetAdmission(directAdmissionFixture{})
	f.request.Capabilities = []string{protocol.ReattachAfterCleanupCapability}
	f.result, err = f.remote.RegisterSession(access.WithGrant(t.Context(), g), f.request)
	if err != nil || f.result.Code != "" {
		t.Fatal("auto registration", err, f.result.Code)
	}
	if err := confirmDirect(t, g, f.result); err != nil {
		t.Fatal(err)
	}
	f.proof = access.DetachedRemovalProof{RegistrationID: f.result.Session.RegistrationID, Revision: f.result.Session.Revision, Identity: bindingForDirect(f.request), OwnerID: g.OwnerID, Name: f.result.Session.Name, AdmissionMode: g.AdmissionMode, GrantID: g.TokenID, ParentID: g.ParentID, Detached: true, ReattachAfterCleanup: true}
	return f
}
func (f *cleanupFixture) revoked() bool {
	for _, t := range f.store.data.Projects["demo"].Tokens {
		if t.ID == f.grant.TokenID {
			return t.Revoked
		}
	}
	return false
}
func (f *cleanupFixture) audits() int {
	n := 0
	for _, e := range f.store.audits {
		if e.Kind == "revoked" && e.Reason == "explicit-release" && e.GrantID == f.grant.TokenID {
			n++
		}
	}
	return n
}
func (f *cleanupFixture) detach(t *testing.T) {
	t.Helper()
	if err := f.remote.StopSession(access.WithGrant(t.Context(), f.grant), f.result.Session.RegistrationID); err != nil {
		t.Fatal(err)
	}
}

func TestDetachedCleanupExactGrantAuditAndWatcher(t *testing.T) {
	f := detachedCleanupFixture(t)
	raw, err := f.store.IssueToken("demo", "unused-replacement")
	if err != nil {
		t.Fatal(err)
	}
	replacement := directGrant(t, f.store, raw)
	before := slices.Clone(f.store.data.Projects["demo"].Tokens)
	f.detach(t)
	if err := f.remote.RemoveDetachedSession(t.Context(), f.result.Session.RegistrationID, nil); err != nil {
		t.Fatal(err)
	}
	if !f.revoked() || f.audits() != 1 {
		t.Fatal("exact cleanup/audit missing")
	}
	p := f.store.data.Projects["demo"]
	for _, b := range p.AutoBindings {
		if b.GrantID == f.grant.TokenID {
			t.Fatal("binding retained")
		}
	}
	for _, e := range p.AutoExchanges {
		if e.GrantID == f.grant.TokenID {
			t.Fatal("exchange retained")
		}
	}
	for i, old := range before {
		if old.ID != f.grant.TokenID && p.Tokens[i].Revoked != old.Revoked {
			t.Fatal("parent or replacement revoked")
		}
	}
	select {
	case <-f.grant.Revoked:
	default:
		t.Fatal("affected watcher not closed")
	}
	select {
	case <-replacement.Revoked:
		t.Fatal("unrelated watcher closed")
	default:
	}
	if err := f.store.CleanupDetachedAutoBatch("demo", f.store.ProjectGeneration("demo"), []access.DetachedRemovalProof{f.proof}); err != nil || f.audits() != 1 {
		t.Fatal("retry duplicated audit")
	}
}

func TestDetachedCleanupRejectsContradictoryOrIneligibleProof(t *testing.T) {
	for _, mode := range []string{"not-detached", "no-capability", "manual-record", "legacy-record", "wrong-grant", "wrong-parent", "wrong-owner", "wrong-identity", "wrong-generation", "claiming", "issued", "other-registration", "pending-rename", "handover-pending", "manual-token", "project-token", "replacement-binding"} {
		t.Run(mode, func(t *testing.T) {
			f := detachedCleanupFixture(t)
			r := f.proof
			p := f.store.data.Projects["demo"]
			generation := p.ProjectGenerationID
			switch mode {
			case "not-detached":
				r.Detached = false
			case "no-capability":
				r.ReattachAfterCleanup = false
			case "manual-record":
				r.AdmissionMode = "manual"
			case "legacy-record":
				r.AdmissionMode = ""
			case "wrong-grant":
				r.GrantID = "unrelated"
			case "wrong-parent":
				r.ParentID = "unrelated"
			case "wrong-owner":
				r.OwnerID = "unrelated"
			case "wrong-identity":
				r.Identity = "unrelated"
			case "wrong-generation":
				generation = "old-generation"
			case "claiming":
				p.AutoBindings[0].State = "claiming"
			case "issued":
				p.AutoBindings[0].State = "issued"
			case "other-registration":
				p.AutoBindings[0].RegistrationID = "different"
			case "pending-rename":
				p.AutoBindings[0].PendingRevision = 2
			case "handover-pending":
				p.AutoBindings[0].DetachPending = true
			case "replacement-binding":
				b := p.AutoBindings[0]
				b.GrantID = "unused-replacement"
				p.AutoBindings = append(p.AutoBindings, b)
			default:
				for i, v := range p.Tokens {
					if v.ID == r.GrantID {
						switch mode {
						case "manual-token":
							p.Tokens[i].AdmissionMode = "manual"
						case "project-token":
							p.Tokens[i].Kind = tokenProject
						}
					}
				}
			}
			f.store.data.Projects["demo"] = p
			if err := f.store.CleanupDetachedAutoBatch("demo", generation, []access.DetachedRemovalProof{r}); err != nil {
				t.Fatal(err)
			}
			if f.revoked() || f.audits() != 0 {
				t.Fatal("ineligible proof changed grant")
			}
		})
	}
}

func TestDetachedCleanupPolicyFailureRetainsRecordForRetry(t *testing.T) {
	f := detachedCleanupFixture(t)
	f.detach(t)
	write := f.store.writeBatch
	f.store.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
	if err := f.remote.RemoveDetachedSession(t.Context(), f.result.Session.RegistrationID, nil); !errors.Is(err, ErrStorage) {
		t.Fatal("control failure ignored", err)
	}
	if f.revoked() || f.audits() != 0 {
		t.Fatal("policy failure published partial policy")
	}
	if _, err := f.remote.GetSession(t.Context(), f.result.Session.RegistrationID); err != nil {
		t.Fatal("policy failure deleted record")
	}
	f.store.writeBatch = write
	if err := f.remote.RemoveDetachedSession(t.Context(), f.result.Session.RegistrationID, nil); err != nil {
		t.Fatal(err)
	}
	if !f.revoked() || f.audits() != 1 {
		t.Fatal("retry did not commit once")
	}
}

// Darwin publishes a snapshot; native Windows SQLite checks its journal path.
func blockBrokerWrite(t *testing.T, root string) func() {
	t.Helper()
	if runtime.GOOS != "windows" && os.Getuid() == 0 {
		t.Skip("requires unprivileged directory permissions")
	}
	var restore func()
	if runtime.GOOS == "windows" {
		journal := filepath.Join(root, "registrations.db-journal")
		if err := os.Mkdir(journal, 0700); err != nil {
			t.Fatal("obstruct journal")
		}
		restore = func() {
			if err := os.Remove(journal); err != nil {
				t.Error("clear journal obstruction")
			}
		}
	} else {
		if err := os.Chmod(root, 0500); err != nil {
			t.Fatal("chmod")
		}
		restore = func() {
			if err := os.Chmod(root, 0700); err != nil {
				t.Error("restore permissions")
			}
		}
	}
	var once sync.Once
	clear := func() { once.Do(restore) }
	t.Cleanup(clear)
	return clear
}

func TestDetachedCleanupBrokerFailureRestartIsIdempotent(t *testing.T) {
	f := detachedCleanupFixture(t)
	f.detach(t)
	restore := blockBrokerWrite(t, f.root)
	if err := f.remote.RemoveDetachedSession(t.Context(), f.result.Session.RegistrationID, nil); err == nil {
		t.Fatal("broker failure accepted")
	}
	if !f.revoked() || f.audits() != 1 {
		t.Fatal("safe partial state missing")
	}
	if _, err := f.remote.GetSession(t.Context(), f.result.Session.RegistrationID); err != nil {
		t.Fatal("failed deletion lost record")
	}
	if err := f.remote.Close(); err != nil {
		t.Fatal(err)
	}
	restore()
	generation := f.store.ProjectGeneration("demo")
	var err error
	f.remote, err = broker.NewSQLitePersistentRemoteServiceWithBatchCleanup(f.root, func(proofs []access.DetachedRemovalProof) error {
		return f.store.CleanupDetachedAutoBatch("demo", generation, proofs)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.remote.RemoveDetachedSession(t.Context(), f.result.Session.RegistrationID, nil); err != nil {
		t.Fatal(err)
	}
	if f.audits() != 1 {
		t.Fatal("restart retry duplicated audit")
	}
}

func TestDetachedCleanupSerializesPreauthenticatedRegistration(t *testing.T) {
	f := detachedCleanupFixture(t)
	f.detach(t)
	entered, release := make(chan struct{}), make(chan struct{})
	remove := make(chan error, 1)
	go func() {
		remove <- f.remote.RemoveDetachedSession(t.Context(), f.result.Session.RegistrationID, func(broker.Session) error { close(entered); <-release; return nil })
	}()
	<-entered
	started := make(chan struct{})
	result := make(chan broker.RegisterResult, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		out, _ := f.remote.RegisterSession(access.WithGrant(t.Context(), f.grant), f.request)
		result <- out
	}()
	<-started
	close(release)
	if err := <-remove; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if out := <-result; out.Code == "" {
		t.Fatal("preauthenticated revoked grant recreated registration")
	}
	if !f.revoked() || f.audits() != 1 {
		t.Fatal("serialized cleanup failed")
	}
}

func TestDetachedCleanupU10bClaimBeforeBrokerWriteFailure(t *testing.T) {
	s := directStore(t)
	parent := directParent(t, s, 8)
	issued, err := s.ExchangeDirect("demo", parent, directRequest(directForm()), DirectProof{Absent: true}, "synthetic", false)
	if err != nil {
		t.Fatal(err)
	}
	g := directGrant(t, s, issued.AccessToken)
	root := t.TempDir()
	generation := s.ProjectGeneration("demo")
	remote, err := broker.NewSQLitePersistentRemoteServiceWithBatchCleanup(root, func(proofs []access.DetachedRemovalProof) error {
		return s.CleanupDetachedAutoBatch("demo", generation, proofs)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	remote.SetAdmission(directAdmissionFixture{})
	req := directRegisterRequest()
	req.Capabilities = []string{protocol.ReattachAfterCleanupCapability}
	restore := blockBrokerWrite(t, root)
	out, err := remote.RegisterSession(access.WithGrant(t.Context(), g), req)
	if err != nil || out.Code != broker.CodeStorage {
		t.Fatal("claim-before-write fixture", err, out.Code)
	}
	p := s.data.Projects["demo"]
	if len(p.AutoBindings) != 1 || p.AutoBindings[0].State != "claiming" {
		t.Fatal("claim evidence lost")
	}
	for _, token := range p.Tokens {
		if token.ID == g.TokenID && token.Revoked {
			t.Fatal("unpersisted registration grant revoked")
		}
	}
	restore()
	out, err = remote.RegisterSession(access.WithGrant(t.Context(), g), req)
	if err != nil || out.Code != "" || confirmDirect(t, g, out) != nil {
		t.Fatal("claim retry could not finish", err, out.Code)
	}
	if len(s.data.Projects["demo"].AutoBindings) != 1 || s.data.Projects["demo"].AutoBindings[0].State != "confirmed" {
		t.Fatal("claim retry duplicated allocation")
	}
}

func TestDetachedCleanupExplicitRevocationInterleavings(t *testing.T) {
	for _, retire := range []bool{false, true} {
		t.Run(map[bool]string{false: "panel-detach", true: "retire"}[retire], func(t *testing.T) {
			f := detachedCleanupFixture(t)
			f.detach(t)
			managerRaw, err := f.store.IssueToken("demo", "manager")
			if err != nil {
				t.Fatal(err)
			}
			manager := directGrant(t, f.store, managerRaw)
			start := make(chan struct{})
			done := make(chan struct{}, 2)
			go func() {
				defer func() { done <- struct{}{} }()
				<-start
				_ = f.remote.RemoveDetachedSession(t.Context(), f.result.Session.RegistrationID, nil)
			}()
			go func() {
				defer func() { done <- struct{}{} }()
				<-start
				if retire {
					_, _ = f.remote.RetireManaged(access.WithGrant(t.Context(), manager), protocol.RetireRequest{RegistrationID: f.result.Session.RegistrationID}, func(target broker.Session, _ string, _ []broker.Session) (protocol.ManagementEvent, error) {
						return protocol.ManagementEvent{ActorName: "manager"}, nil
					}, manager.Retire)
				} else {
					_ = f.remote.RemoveDetachedSession(t.Context(), f.result.Session.RegistrationID, func(current broker.Session) error {
						return f.store.RevokeSessionTokens("demo", current.Name, f.proof.Identity)
					})
				}
			}()
			close(start)
			<-done
			<-done
			if !f.revoked() {
				t.Fatal("competing explicit cleanup kept target grant")
			}
			if err := f.store.ReleaseDirectRegistration("demo", f.proof.Identity); err != nil {
				t.Fatal(err)
			}
			if f.audits() != 1 {
				t.Fatal("competing cleanup duplicated explicit-release audit")
			}
			select {
			case <-manager.Revoked:
				t.Fatal("unrelated manager credential canceled")
			default:
			}
		})
	}
}

func TestDetachedCleanupPanelBaselineAddsNoF1Audit(t *testing.T) {
	f := detachedCleanupFixture(t)
	f.detach(t)
	if err := f.store.RevokeSessionTokens("demo", f.proof.Name, f.proof.Identity); err != nil {
		t.Fatal(err)
	}
	count := len(f.store.audits)
	if err := f.store.CleanupDetachedAutoBatch("demo", f.store.ProjectGeneration("demo"), []access.DetachedRemovalProof{f.proof}); err != nil {
		t.Fatal(err)
	}
	if len(f.store.audits) != count {
		t.Fatal("F1 audited a panel-revoked grant")
	}
	if err := f.remote.RemoveDetachedSession(t.Context(), f.result.Session.RegistrationID, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.store.audits) != count {
		t.Fatal("pre-removal hook added panel audit")
	}
	if err := f.store.ReleaseDirectRegistration("demo", f.proof.Identity); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, e := range f.store.audits {
		if e.GrantID == f.grant.TokenID && e.Kind == "revoked" {
			kinds[e.Reason]++
		}
	}
	if kinds["superseded"] != 0 || kinds["explicit-release"] != 1 {
		t.Fatal("panel revocation was not recorded exactly once")
	}
}

func TestPanelDetachRecordFailureListingKeepsOneRevocation(t *testing.T) {
	f := detachedCleanupFixture(t)
	f.detach(t)
	var restore func()
	err := f.remote.RemoveDetachedSession(t.Context(), f.result.Session.RegistrationID, func(current broker.Session) error {
		if err := f.store.RevokeSessionTokens("demo", current.Name, f.proof.Identity); err != nil {
			return err
		}
		restore = blockBrokerWrite(t, f.root)
		return nil
	})
	if err == nil {
		t.Fatal("registration deletion did not fail")
	}
	if !f.revoked() || f.audits() != 1 {
		t.Fatal("revocation was not audited at the policy commit")
	}
	for _, event := range f.store.audits {
		if event.GrantID == f.grant.TokenID && event.Kind == "revoked" && event.Reason != "explicit-release" {
			t.Fatal("incorrect panel revocation reason")
		}
	}
	if _, err := f.remote.GetSession(t.Context(), f.result.Session.RegistrationID); err != nil {
		t.Fatal("failed deletion lost registration")
	}
	restore()
	f.remote.SetProbeSchedule(time.Nanosecond, time.Nanosecond, 0)
	rows, err := f.remote.ListSessions(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatal("next listing did not retry record removal")
	}
	if f.audits() != 1 {
		t.Fatal("listing duplicated revocation audit")
	}
	if f.store.ReleaseDirectRegistration("demo", f.proof.Identity) != nil || f.audits() != 1 {
		t.Fatal("binding cleanup duplicated revocation audit")
	}
	if len(f.store.data.Projects["demo"].AutoBindings) != 0 {
		t.Fatal("audited binding was not removed")
	}
}

func TestPanelDetachDirectReissueKeepsOneRevocation(t *testing.T) {
	f := detachedCleanupFixture(t)
	if f.store.RevokeSessionTokens("demo", f.proof.Name, f.proof.Identity) != nil {
		t.Fatal("panel revocation")
	}
	form := directForm()
	form.Set("request_id", "after-panel-detach")
	if _, err := f.store.ExchangeDirect("demo", f.parent, directRequest(form), DirectProof{Absent: true}, "fixture", false); err != nil {
		t.Fatal("direct reissue")
	}
	if f.audits() != 1 {
		t.Fatal("direct reissue duplicated old grant revocation")
	}
	for _, event := range f.store.audits {
		if event.GrantID == f.grant.TokenID && event.Kind == "revoked" && event.Reason != "explicit-release" {
			t.Fatal("direct reissue retained an incorrect revocation reason")
		}
	}
}

func TestDetachedCleanupPendingHandoverCommitBoundaries(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "policy-committed", true: "broker-applied-before-confirm"}[applied], func(t *testing.T) {
			f := detachedCleanupFixture(t, "claude")
			next := f.request
			next.SessionID = "22222222-2222-4222-8222-222222222222"
			next.RequestID = "pending-handover"
			next.ExpectedRevision = f.result.Session.Revision
			proof, err := f.remote.InspectConversationHandover(t.Context(), next, f.result.Session.RegistrationID)
			if err != nil {
				t.Fatal("handover inspection", err)
			}
			request := DirectRequest{DeviceRequest: DeviceRequest{ClientID: "asb-client-v1", Harness: next.Harness, SessionID: next.SessionID, InstanceID: next.ClientID, RequestID: next.RequestID}, Handover: true, PID: next.PID, CWD: next.CWD, RegistrationID: f.result.Session.RegistrationID, ResumeSecret: next.Secret}
			issued, err := f.store.ExchangeDirect("demo", f.parent, request, DirectProof{RegistrationID: proof.RegistrationID, OwnerID: proof.OwnerID, Name: proof.Name, Revision: proof.Revision, AdmissionGrantID: proof.AdmissionGrantID, PreviousIdentity: proof.PreviousIdentity}, "synthetic", false)
			if err != nil {
				t.Fatal("handover exchange", err)
			}
			if applied {
				g := directGrant(t, f.store, issued.AccessToken)
				if _, err := f.remote.RebindConversation(access.WithGrant(t.Context(), g), next, f.result.Session.RegistrationID); err != nil {
					t.Fatal("handover apply", err)
				}
			}
			before := slices.Clone(f.store.data.Projects["demo"].Tokens)
			count := len(f.store.audits)
			// Pause before policy confirmation, then an independently authorized stop/list.
			if err := f.remote.StopSession(t.Context(), f.result.Session.RegistrationID); err != nil {
				t.Fatal(err)
			}
			if err := f.remote.RemoveDetachedSession(t.Context(), f.result.Session.RegistrationID, nil); err != nil {
				t.Fatal(err)
			}
			after := f.store.data.Projects["demo"].Tokens
			for i, v := range before {
				if after[i].Revoked != v.Revoked {
					t.Fatal("pending handover cleanup revoked a grant")
				}
			}
			if len(f.store.audits) != count {
				t.Fatal("pending handover added cleanup audit")
			}
		})
	}
}

func TestDetachedCleanupPendingRenamePolicyCommit(t *testing.T) {
	f := detachedCleanupFixture(t)
	request := directRequest(directForm())
	request.RequestID = "rename-journal"
	request.RegistrationID = f.result.Session.RegistrationID
	request.ResumeSecret = f.request.Secret
	request.RequestedNickname = "pending-name"
	next, err := f.store.ExchangeDirect("demo", f.parent, request, brokerProof(t, f.remote, request), "synthetic", false)
	if err != nil {
		t.Fatal(err)
	}
	grant := directGrant(t, f.store, next.AccessToken)
	write := f.store.write
	calls := 0
	f.store.write = func(data []byte) error {
		calls++
		if calls == 2 {
			return ErrStorage
		}
		return write(data)
	}
	_, err = f.remote.RenameDirectRegistration(access.WithGrant(t.Context(), grant), request.RegistrationID, request.ResumeSecret, next.Revision, request.RequestedNickname)
	f.store.write = write
	if err == nil {
		t.Fatal("rename final commit fault not injected")
	}
	before := slices.Clone(f.store.data.Projects["demo"].Tokens)
	count := len(f.store.audits)
	if err := f.remote.StopSession(t.Context(), f.result.Session.RegistrationID); err != nil {
		t.Fatal(err)
	}
	if err := f.remote.RemoveDetachedSession(t.Context(), f.result.Session.RegistrationID, nil); err != nil {
		t.Fatal(err)
	}
	after := f.store.data.Projects["demo"].Tokens
	for i, v := range before {
		if after[i].Revoked != v.Revoked {
			t.Fatal("pending rename revoked a grant")
		}
	}
	if len(f.store.audits) != count {
		t.Fatal("pending rename added audit")
	}
}

func TestDetachedCleanupReservedRowInvariantAndSurvivorDelivery(t *testing.T) {
	f := detachedCleanupFixture(t)
	if _, err := f.store.IssueToken("demo", "manual-orphan"); err != nil {
		t.Fatal(err)
	}
	raw, err := f.store.IssueToken("demo", "live-sibling")
	if err != nil {
		t.Fatal(err)
	}
	g := directGrant(t, f.store, raw)
	req := directRegisterRequest()
	req.SessionID = "33333333-3333-4333-8333-333333333333"
	req.ClientID = "sibling-client"
	req.RequestID = "sibling-request"
	req.ChannelID = "sibling-lease"
	live, err := f.remote.RegisterSession(access.WithGrant(t.Context(), g), req)
	if err != nil || live.Code != "" {
		t.Fatal("sibling registration", err, live.Code)
	}
	f.detach(t)
	if err := f.remote.RemoveDetachedSession(t.Context(), f.result.Session.RegistrationID, nil); err != nil {
		t.Fatal(err)
	}
	sessions, err := f.remote.ListSessions(t.Context())
	if err != nil || len(sessions) != 1 {
		t.Fatal("active row count changed")
	}
	var panel []Session
	for _, s := range sessions {
		panel = append(panel, Session{ID: s.RegistrationID, RegistrationID: s.RegistrationID, SessionID: s.ID, Name: s.Name, Harness: s.Harness, ClientID: s.ClientID})
	}
	reserved := reservedSessionRows(f.store.data.Projects["demo"], f.store.data.Users, panel)
	if len(reserved) != 1 || reserved[0].Name != "manual-orphan" {
		t.Fatal("reserved-row invariant violated")
	}
	// Policy changes invalidate request grants; the next request reauthorizes the survivor.
	g = directGrant(t, f.store, raw)
	ctx := access.WithGrant(t.Context(), g)
	if out, err := f.remote.SendMessage(ctx, broker.Message{From: live.Session.Name, To: live.Session.Name, Body: "surviving-holder"}); err != nil || !out.Accepted {
		t.Fatal("sibling could not send")
	}
	message, err := f.remote.WaitForIngress(ctx, live.Session.Name, req.ChannelID)
	if err != nil || message.Body != "surviving-holder" {
		t.Fatal("sibling could not receive")
	}
}
