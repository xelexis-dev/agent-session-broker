package channel

import (
	"context"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type followStartupKey struct{}

func (f *claudeFollow) startupRetryBlocked(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.retryID == id && followPollClock(f.opts).now().Before(f.retryAt)
}
func autoSavedConversation(s clientstate.SessionState) bool {
	return s.LastAdmissionMode == "auto" || (s.Credential != nil && s.Credential.AdmissionMode == "auto")
}

func (f *claudeFollow) installUnregistered(o durableOptions, attach func(context.Context, *mcp.CallToolRequest) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startup = func(ctx context.Context, id broker.AttachRequest) (broker.AttachRequest, error) {
		// The startup worker already owns this resume; the monitor must not fence it
		// using the previous parent's PID before the broker checks the receiver holder.
		if f.lookup != nil {
			if pending := f.lookup(id.SessionID); pending != nil {
				pending.mu.Lock()
				started, d := pending.started, pending.durable
				pending.mu.Unlock()
				if started {
					return id, nil
				}
				if d != nil && d.session != nil {
					d.session.mu.Lock()
					busy := d.session.registrationAttempts > 0
					d.session.mu.Unlock()
					if busy {
						return id, nil
					}
				}
			}
		}

		cid, err := o.state.ClientID()
		if err != nil {
			return id, err
		}
		key := clientstate.SessionKey(o.scope, id.Harness, id.SessionID, cid)
		saved, found, err := o.state.LoadSession(key)
		if err != nil {
			return id, err
		}
		if !found {
			return id, nil
		}
		if saved.Scope != o.scope || saved.Harness != "claude" || saved.ClientID != cid || saved.SessionID != id.SessionID {
			return id, broker.HandoverRefusal{Reason: "refused"}
		}
		if saved.Registration.Detached {
			return id, broker.HandoverRefusal{Reason: "detached"}
		}
		if saved.Registration.RegistrationID == "" && saved.Registration.HandedOverTo == "" {
			return id, nil
		}
		mode := saved
		if saved.Registration.RegistrationID != "" || saved.Registration.HandedOverTo != "" {
			holder, e := o.state.HandoverHolder(key, o.scope, "claude", cid, false)
			if e != nil {
				return id, broker.HandoverRefusal{Reason: handoverResolutionReason(e)}
			}
			mode = holder.State
			saved.Registration.HandedOverTo = ""
			if holder.Key != key {
				saved.Registration.HandedOverTo = holder.Key
			}
		}
		if !autoSavedConversation(mode) {
			if mode.LastAdmissionMode == "manual" {
				return id, handoverError("approval-required")
			}
			return id, nil
		}
		// Resume itself retries suspended leases indefinitely. Read the existing
		// target first so a refusal uses this monitor's bounded 30-second retry.
		if saved.Registration.HandedOverTo == "" && saved.Credential != nil {
			probe := o.device.newUpstream(saved.Credential.AccessToken)
			var remote broker.Session
			err := call(ctx, probe, "get_session", map[string]string{"name": broker.SelectorRegistration + saved.Registration.RegistrationID}, &remote)
			suspended, rejected := upstreamSuspended(probe), upstreamRejected(probe)
			closeUpstream(probe)
			if suspended || remote.Suspended {
				return id, broker.HandoverRefusal{Reason: "suspended"}
			}
			if remote.ErrorCode == broker.CodeDetached {
				return id, broker.HandoverRefusal{Reason: "detached"}
			}
			if err != nil && !rejected {
				return id, err
			}
			if remote.Status == broker.StatusAlive && remote.PID != id.PID {
				return id, broker.HandoverRefusal{Reason: "still-receiving"}
			}
		}
		callCtx := context.WithValue(context.WithValue(ctx, followIdentityKey{}, id), followStartupKey{}, true)
		if err := attach(callCtx, nil); err != nil {
			if b := f.lookup(id.SessionID); b != nil {
				if d := o.diagnostics(b); d != nil && d.HandoverSkipped != "" {
					return id, broker.HandoverRefusal{Reason: d.HandoverSkipped}
				}
				b.mu.Lock()
				reason := b.resumeStopReason
				b.mu.Unlock()
				if reason == "registration-detached" {
					return id, broker.HandoverRefusal{Reason: "detached"}
				}
			}
			return id, err
		}
		b := f.lookup(id.SessionID)
		if b == nil {
			return id, errors.New("resumed binding is unavailable")
		}
		b.mu.Lock()
		next, started := b.identity, b.started && !b.failed
		b.mu.Unlock()
		if !started {
			return id, errors.New("resumed binding is unavailable")
		}
		f.result = "followed: resumed"
		if saved.Registration.HandedOverTo != "" {
			f.result = "followed: handover"
		}
		return next, nil
	}
}
