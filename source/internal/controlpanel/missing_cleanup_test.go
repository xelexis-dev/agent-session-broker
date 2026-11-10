package controlpanel

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func inactiveCleanupFixture(t *testing.T, mode string) *cleanupFixture {
	t.Helper()
	f := detachedCleanupFixture(t)
	f.detach(t)
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	p := f.store.data.Projects["demo"]
	for i, v := range p.Tokens {
		if v.ID == f.grant.ParentID {
			p.Tokens[i].ExpiresAt = time.Time{}
		}
	}
	switch mode {
	case "expired":
		future := f.store.now().Add(31 * 24 * time.Hour)
		f.store.now = func() time.Time { return future }
	case "revoked":
		for i, v := range p.Tokens {
			if v.ID == f.grant.TokenID {
				p.Tokens[i].Revoked = true
				p.Tokens[i].RevokedAt = f.store.now()
			}
		}
	case "absent":
		p.Tokens = slices.DeleteFunc(p.Tokens, func(v SessionToken) bool { return v.ID == f.grant.TokenID })
	}
	f.store.data.Projects["demo"] = p
	return f
}
func TestMissingCleanupInactiveAutoAndReauthorize(t *testing.T) {
	for _, mode := range []string{"expired", "revoked", "absent"} {
		t.Run(mode, func(t *testing.T) {
			f := inactiveCleanupFixture(t, mode)
			f.store.mu.Lock()
			originalWrite := f.store.writeBatch
			before, beforeAudits := f.storeSnapshotForMissingTest()
			f.store.writeBatch = func(map[string][]byte, []string) error { return ErrStorage }
			f.store.mu.Unlock()
			if e := f.remote.RemoveDetachedSession(t.Context(), f.result.Session.RegistrationID, nil); e == nil {
				t.Fatal("inactive cleanup ignored policy save failure")
			}
			f.store.mu.Lock()
			after, afterAudits := f.storeSnapshotForMissingTest()
			f.store.writeBatch = originalWrite
			f.store.mu.Unlock()
			if !reflect.DeepEqual(before, after) || beforeAudits != afterAudits {
				t.Fatal("failed inactive cleanup changed policy")
			}
			if _, e := f.remote.GetSession(t.Context(), f.result.Session.RegistrationID); e != nil {
				t.Fatal("failed policy save removed broker record")
			}
			if e := f.remote.RemoveDetachedSession(t.Context(), f.result.Session.RegistrationID, nil); e != nil {
				t.Fatal(e)
			}
			f.store.mu.RLock()
			p := f.store.data.Projects["demo"]
			bindings, exchanges := len(p.AutoBindings), len(p.AutoExchanges)
			f.store.mu.RUnlock()
			if bindings != 0 || exchanges != 0 || f.audits() != 1 {
				t.Fatal("inactive auto binding/exchange not released exactly once")
			}
			for _, v := range p.Tokens {
				if v.ID == f.grant.TokenID && mode == "expired" && v.Revoked {
					t.Fatal("expired token was needlessly revoked")
				}
			}
			form := directForm()
			form.Set("request_id", "after-cleanup")
			issued, e := f.store.ExchangeDirect("demo", f.parent, directRequest(form), DirectProof{Absent: true}, "synthetic", false)
			if e != nil || issued.AccessToken == "" {
				t.Fatal("same identity cannot reauthorize")
			}
			g := directGrant(t, f.store, issued.AccessToken)
			req := f.request
			req.RequestID = "after-cleanup-register"
			req.ChannelID = "after-cleanup-lease"
			req.ExpectedRevision = 0
			out, e := f.remote.RegisterSession(access.WithGrant(t.Context(), g), req)
			if e != nil || out.Code != "" || out.Session.RegistrationID == f.result.Session.RegistrationID {
				t.Fatal("same identity cannot get new registration")
			}
			if e = confirmDirect(t, g, out); e != nil {
				t.Fatal(e)
			}
			if e = f.store.CleanupDetachedAutoBatch("demo", f.store.ProjectGeneration("demo"), []access.DetachedRemovalProof{f.proof}); e != nil || f.audits() != 1 {
				t.Fatal("old proof replay changed new grant")
			}
			if len(f.store.data.Projects["demo"].AutoBindings) != 1 {
				t.Fatal("old proof removed replacement binding")
			}
		})
	}
}
func TestMissingCleanupInactiveProofFences(t *testing.T) {
	for _, mode := range []string{"expired", "revoked", "absent"} {
		cases := []string{"manual", "legacy", "parent", "owner", "identity", "registration", "pending"}
		if mode != "absent" {
			cases = append(cases, "name")
		}
		for _, bad := range cases {
			t.Run(mode+"/"+bad, func(t *testing.T) {
				f := inactiveCleanupFixture(t, mode)
				r := f.proof
				f.store.mu.Lock()
				p := f.store.data.Projects["demo"]
				switch bad {
				case "manual":
					r.AdmissionMode = "manual"
				case "legacy":
					r.AdmissionMode = ""
				case "parent":
					r.ParentID = "wrong"
				case "owner":
					r.OwnerID = "wrong"
				case "identity":
					r.Identity = "wrong"
				case "registration":
					r.RegistrationID = "wrong"
				case "pending":
					p.AutoBindings[0].PendingRevision = 7
				case "name":
					r.Name = "wrong"
				}
				f.store.data.Projects["demo"] = p
				before, beforeAudits := f.storeSnapshotForMissingTest()
				f.store.mu.Unlock()
				if e := f.store.CleanupDetachedAutoBatch("demo", f.store.ProjectGeneration("demo"), []access.DetachedRemovalProof{r}); e != nil {
					t.Fatal(e)
				}
				f.store.mu.RLock()
				after, afterAudits := f.storeSnapshotForMissingTest()
				f.store.mu.RUnlock()
				if !reflect.DeepEqual(before, after) || beforeAudits != afterAudits {
					t.Fatal("ineligible proof changed control state")
				}
			})
		}
	}
}
func (f *cleanupFixture) storeSnapshotForMissingTest() (Project, int) {
	p := f.store.data.Projects["demo"]
	p.Tokens = slices.Clone(p.Tokens)
	p.AutoBindings = slices.Clone(p.AutoBindings)
	p.AutoExchanges = slices.Clone(p.AutoExchanges)
	return p, len(f.store.audits)
}
