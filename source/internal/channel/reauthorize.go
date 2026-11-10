package channel

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

var (
	errDirectUnsupported    = errors.New("direct-unsupported: The broker does not support automatic direct reauthorization")
	errDirectPolicy         = errors.New("direct-policy: The broker policy refused automatic direct reauthorization")
	errRecoveryRegistration = errors.New("registration-invalid: Check the saved registration before reconnecting")
)

// Only this type is retryable. A sanitized errDirectFailed on a malformed grant
// or permanent 4xx must never be mistaken for a transport failure.
type directTransientError struct{ retryAfter time.Duration }

func (e *directTransientError) Error() string {
	return "direct-transient: The broker is temporarily unavailable"
}
func (e *directTransientError) Unwrap() error { return errDirectFailed }

// HTTP-date retry deadlines and Date share the server's clock. Comparing
// them directly to a skewed local wall clock can turn a short delay into a
// permanent rate-limit failure. Delta-seconds and missing Date retain their
// existing behavior; malformed headers are never treated as proof of time.
func responseRetryReference(header http.Header, fallback time.Time) time.Time {
	if serverNow, err := http.ParseTime(header.Get("Date")); err == nil {
		return serverNow
	}
	return fallback
}

func recoveryRetryAfter(raw string, now time.Time) (time.Duration, error) {
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 && seconds <= int64((1<<63-1)/int64(time.Second)) {
		return time.Duration(seconds) * time.Second, nil
	}
	if at, err := http.ParseTime(raw); err == nil && !at.Before(now) {
		return at.Sub(now), nil
	}
	return 0, errDirectRate
}

func automaticRecoveryBackoff(attempt int) time.Duration {
	return min(30*time.Second<<uint(min(max(attempt, 0), 5)), 600*time.Second)
}

func (a *sessionAuth) autoEligibleLocked() bool {
	return a.state.LastAdmissionMode == "auto" || (a.state.Credential != nil && a.state.Credential.AdmissionMode == "auto")
}

// A definitive stop never grants an automatic attempt, even if the parent changes.
func (a *sessionAuth) automaticStoppedLocked() bool {
	if a.state.Registration.Detached {
		return true
	}
	if r := a.state.AutoRecovery; r != nil {
		switch r.StopReason {
		case "registration-detached", "registration-not-found", "registration-owner-mismatch":
			return true
		}
	}
	return false
}

func autoRecoveryGuidance(reason string) string {
	switch reason {
	case "reattach-refused":
		return errReattachRefused.Error()
	case "project-deleted", "project_deleted", "project_not_found":
		return (&projectRefusalError{code: reason, terminal: true}).Error()
	case "registration-capacity":
		return "automatic-reauthorization-stopped: " + registrationCapacityGuidance
	case "project-token-rejected":
		return "automatic-reauthorization-stopped: The broker rejected the project admission token. Replace the project token in the MCP configuration, then reconnect this client with the same local state directory."
	case "registration-detached":
		return "automatic-reauthorization-stopped: registration-detached. " + detachedGuidance
	case "registration-not-found":
		return "automatic-reauthorization-stopped: registration-not-found. The broker no longer has this saved registration; an administrator may have detached it, another session may have retired it, or it was removed. No new registration was created. Call attach_session explicitly to register again."
	case "registration-invalid", "registration-owned", "registration-revision-mismatch":
		return "automatic-reauthorization-stopped: " + reason + ". Check the saved registration and its ownership before reconnecting. No new registration was created."
	case "registration-owner-mismatch":
		return "automatic-reauthorization-stopped: registration-owner-mismatch. This token's owner differs from the saved registration's owner. Ask an administrator to detach the previous registration, then call attach_session explicitly."
	case "identity-conflict":
		return "automatic-reauthorization-stopped: " + errDirectConflict.Error()
	case "pending-expired":
		return "automatic-reauthorization-stopped: " + errDirectPendingExpired.Error()
	default:
		return "automatic-reauthorization-stopped: " + reason + ". Check the broker's direct admission support, token scopes and policy, then reconnect. No approval code was requested."
	}
}

func recoveryStopReason(err error) string {
	if errors.Is(err, errReattachRefused) {
		return "reattach-refused"
	}
	var refusal broker.HandoverRefusal
	if errors.As(err, &refusal) {
		switch refusal.Reason {
		case broker.CodeNotFound:
			return "registration-not-found"
		case "owner-mismatch":
			return "registration-owner-mismatch"
		}
	}
	switch {
	case terminalProjectError(err):
		return "project-deleted"
	case errors.Is(err, errProjectTokenRejected):
		return "project-token-rejected"
	case errors.Is(err, errDirectUnsupported):
		return "direct-unsupported"
	case errors.Is(err, errDirectCapacity):
		return "registration-capacity"
	case errors.Is(err, errDirectPendingExpired):
		return "pending-expired"
	case errors.Is(err, errDirectConflict):
		return "identity-conflict"
	case errors.Is(err, errInsufficientScope):
		return "insufficient-scope"
	case errors.Is(err, errHTTPTransportDenied), errors.Is(err, errDirectPolicy):
		return "direct-policy"
	case errors.Is(err, errRecoveryRegistration):
		return "registration-invalid"
	case errors.Is(err, errDirectRate):
		return "rate-limit-without-valid-retry-after"
	default:
		return "invalid-direct-response"
	}
}

func (o durableOptions) stopAutoRecovery(a *sessionAuth, reason string) {
	a.mu.Lock()
	r := clientstate.AutoRecovery{StopReason: reason}
	if a.state.AutoRecovery != nil {
		r.Attempts = a.state.AutoRecovery.Attempts
		r.Kind = a.state.AutoRecovery.Kind
	}
	if a.state.Registration.RegistrationID == "" {
		a.state.Registration.InitialAttempt = initialStopped
	}
	a.state.AutoRecovery = &r
	a.message = autoRecoveryGuidance(reason)
	a.mu.Unlock()
	_ = o.persistSession(a)
}

// The caller owns the session's recovery slot and exchangeMu. Each exchange is
// persisted before I/O; tools cannot reset the request, delay or terminal state.
func (o durableOptions) reauthorize(ctx context.Context, a *sessionAuth, id broker.AttachRequest) bool {
	clock := o.device.clock
	if clock.now == nil {
		clock.now = time.Now
	}
	if clock.sleep == nil {
		clock.sleep = realPollClock().sleep
	}
	for ctx.Err() == nil {
		a.mu.Lock()
		r := a.state.AutoRecovery
		if r == nil {
			r = &clientstate.AutoRecovery{}
			a.state.AutoRecovery = r
		}
		stopped, next := r.StopReason != "", r.NextAttemptAt
		a.mu.Unlock()
		if stopped {
			return false
		}
		if delay := next.Sub(clock.now()); delay > 0 {
			if clock.sleep(ctx, delay) != nil {
				return false
			}
		}
		a.mu.Lock()
		changed := a.state.AutoRecovery.Kind == "token-changed"
		migration := a.state.AutoRecovery.Kind == "legacy-migration"
		a.mu.Unlock()
		handled, err := o.tryDirectWithPolicy(ctx, a, id, !changed)
		if err == nil && !handled && changed {
			err = o.requestDeviceCode(ctx, a, id)
			if err == nil {
				a.mu.Lock()
				a.state.AutoRecovery = nil
				a.mu.Unlock()
				if o.persistSession(a) != nil {
					o.stopAutoRecovery(a, "local-state-write-failed")
				}
				return false // the one device poller owns continuation
			}
		}
		if migration && err != nil {
			o.stopAutoRecovery(a, recoveryStopReason(err))
			return false
		}
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		var temporary *directTransientError
		if !errors.As(err, &temporary) {
			o.stopAutoRecovery(a, recoveryStopReason(err))
			return false
		}
		a.mu.Lock()
		previous := a.state.AutoRecovery
		delay := max(automaticRecoveryBackoff(previous.Attempts), temporary.retryAfter)
		a.state.AutoRecovery = &clientstate.AutoRecovery{Kind: previous.Kind, Attempts: previous.Attempts + 1, NextAttemptAt: clock.now().Add(delay)}
		a.mu.Unlock()
		if o.persistSession(a) != nil {
			o.stopAutoRecovery(a, "local-state-write-failed")
			return false
		}
	}
	return false
}
