package channel

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
)

func TestHandoverMarkerFailureRestartAndReturnNeverRegistersFresh(t *testing.T) {
	for _, stage := range []string{"before-both-markers", "between-markers"} {
		t.Run(stage, func(t *testing.T) {
			r := newDetachedRuntime(t)
			dir := t.TempDir()
			c0 := claudeHandoverIdentity(981, 9301)
			c1 := claudeHandoverIdentity(982, 9301)
			var fresh atomic.Int32
			options := func(o *durableOptions) {
				handoverOptions(o)
				o.device.authorizer = &returnAuthorizer{o.device.authorizer.(*deviceAuthClient), &fresh}
			}
			initial := r.identityHarness(t, dir, c0, options)
			initial.attach(t)
			original := initial.session
			initial.close()
			_, sourceKey, _ := handoverSaved(t, r, dir, c0)
			faultPath := filepath.Join(dir, "sessions", sourceKey, "handover.json")
			var injected atomic.Bool
			moving := r.identityHarness(t, dir, c1, func(o *durableOptions) {
				options(o)
				o.device.persist = func(store *clientstate.Store, key string, state clientstate.SessionState) error {
					if err := store.SaveSession(key, state); err != nil {
						return err
					}
					if state.Registration.HandoverFrom != nil && state.Credential != nil && injected.CompareAndSwap(false, true) {
						if stage == "before-both-markers" {
							return errors.New("synthetic post-save interruption")
						}
						return os.Mkdir(faultPath, 0700)
					}
					return nil
				}
			})
			result, err := moving.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "attach_session", Arguments: map[string]any{}})
			if err != nil || !result.IsError || !injected.Load() {
				t.Fatal("handover failure fixture did not interrupt marker writes")
			}
			moving.close()
			if stage == "between-markers" {
				if err = os.Remove(faultPath); err != nil {
					t.Fatal(err)
				}
			}
			before := r.direct.Load()
			restarted := r.identityHarness(t, dir, c1, options)
			restarted.attach(t)
			if restarted.session.RegistrationID != original.RegistrationID || r.direct.Load() != before {
				t.Fatal("destination restart created a new registration/exchange")
			}
			restarted.close()
			returned := r.identityHarness(t, dir, c0, options)
			returned.attach(t)
			if returned.session.RegistrationID != original.RegistrationID || returned.session.Name != original.Name || fresh.Load() != 1 {
				t.Fatal("return lost holder or created a fresh registration")
			}
			returned.close()
			again := r.identityHarness(t, dir, c1, options)
			again.attach(t)
			if again.session.RegistrationID != original.RegistrationID || fresh.Load() != 1 {
				t.Fatal("second return changed durable registration")
			}
		})
	}
}
func TestHandoverEqualRevisionRefusesWithoutFreshAdmission(t *testing.T) {
	r := newDetachedRuntime(t)
	dir := t.TempDir()
	c0 := claudeHandoverIdentity(991, 9401)
	c1 := claudeHandoverIdentity(992, 9401)
	first := r.identityHarness(t, dir, c0, handoverOptions)
	first.attach(t)
	first.close()
	next := r.identityHarness(t, dir, c1, handoverOptions)
	next.attach(t)
	next.close()
	store, k0, s0 := handoverSaved(t, r, dir, c0)
	_, k1, s1 := handoverSaved(t, r, dir, c1)
	s0.Registration.Revision = s1.Registration.Revision
	s0.Registration.HandoverFrom = nil
	if err := store.SaveSession(k0, s0); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearHandedOver(k0); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearHandedOver(k1); err != nil {
		t.Fatal(err)
	}
	h := r.identityHarness(t, dir, claudeHandoverIdentity(993, 9401), handoverOptions)
	before := r.direct.Load()
	handoverRefusal(t, h, s1.Registration.Name, "not-found")
	status := h.status(t)
	if r.direct.Load() != before || status.Registered {
		t.Fatal("ambiguous holder allocated a new registration")
	}
	if status.Diagnostics == nil || status.Diagnostics.HandoverSkipped != "not-found" {
		t.Fatal("ambiguous holder diagnostic missing")
	}
}
