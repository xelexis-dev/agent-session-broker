package channel

import (
	"context"
	"encoding/json"
	"errors"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const registrationCapacityGuidance = "registration-capacity: This project token has reached its automatic registration limit. Ask an administrator to detach unused sessions in the control panel or raise the token's limit, then call attach_session again."

var errDirectCapacity = errors.New(registrationCapacityGuidance)
var errDirectConflict = errors.New("identity-conflict: The broker refused the saved registration identity or ownership proof. Check that the current project token belongs to the same owner, then call attach_session explicitly to retry. If the token belongs to a different owner, ask an administrator to detach the previous registration first.")
var errDirectPendingExpired = errors.New("pending-expired: The previous direct request expired after ten minutes. Call attach_session explicitly to start a fresh request.")
var errDirectRate = errors.New("rate-limited: Direct registration was rate limited. Wait before explicitly reconnecting.")
var errDirectFailed = errors.New("direct-registration-failed: Direct registration did not complete. Reconnect explicitly to retry.")
var errScopeUpgrade = errors.New("client-upgrade-required: This restricted credential requires a broker supporting scoped sessions.")

type directAuthorizer interface {
	discover(context.Context) (bool, error)
	exchange(context.Context, directCodeRequest) (directGrant, error)
}
type directCodeRequest struct {
	Holder          *broker.ReceiverHolder
	HolderProof     *broker.HolderProof
	Handover        bool
	PID             int
	CWD, ProcessKey string
	deviceCodeRequest
	Nickname, RegistrationID, Secret string
	Recovery                         bool // one request; the recovery owner schedules retries
}
type directGrant struct {
	HandoverFrom *broker.HandoverFrom `json:"handover_from,omitempty"`
	sessionGrant
	Scopes         []string `json:"scopes"`
	Mode           string   `json:"admission_mode"`
	RegistrationID string   `json:"registration_id"`
	Revision       uint64   `json:"revision"`
}

// Discovery is an optional optimization, not an authorization attempt. Reuse
// the exchange's three-attempt, 1s/2s cancelable backoff for transient failures.
func (c *deviceAuthClient) discoverCapabilities(ctx context.Context) (bool, bool, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if err := c.policy.ensure(ctx); err != nil {
			return false, false, err
		}
		req, err := http.NewRequestWithContext(ctx, "GET", c.origin+"/oauth/capabilities?project="+url.QueryEscape(c.project), nil)
		if err != nil {
			return false, false, errDirectFailed
		}
		// Public discovery never carries a bearer and the client never redirects.
		res, err := c.http.Do(req)
		if err == nil {
			switch {
			case res.StatusCode >= 500:
				res.Body.Close()
			case res.StatusCode != http.StatusOK:
				res.Body.Close()
				return false, false, nil
			default:
				data, readErr := io.ReadAll(io.LimitReader(res.Body, (64<<10)+1))
				res.Body.Close()
				if readErr == nil {
					var v struct {
						Version      int      `json:"version"`
						Capabilities []string `json:"capabilities"`
					}
					if len(data) > 64<<10 || json.Unmarshal(data, &v) != nil || v.Version != 1 {
						return false, false, nil
					}
					c.holderSupported.Store(false)
					c.kimiSupported.Store(false)
					direct, scoped := false, false
					for _, capability := range v.Capabilities {
						if capability == "harness-kimi-v1" {
							c.kimiSupported.Store(true)
						}
						direct = direct || capability == "direct-registration-v1"
						if capability == broker.ReceiverHolderCapability {
							c.holderSupported.Store(true)
						}
						scoped = scoped || capability == "scoped-session-v1"
					}
					return direct, scoped, nil
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		if attempt < 2 {
			if err := c.clock.sleep(ctx, time.Duration(attempt+1)*time.Second); err != nil {
				return false, false, err
			}
		}
	}
	return false, false, errDirectFailed
}
func (c *deviceAuthClient) discover(ctx context.Context) (bool, error) {
	direct, _, _ := c.discoverCapabilities(ctx)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// Manual authorization still performs its own transport and bearer checks.
	return direct, nil
}
func (c *deviceAuthClient) scoped(ctx context.Context) (bool, error) {
	_, scoped, err := c.discoverCapabilities(ctx)
	if canceled := ctx.Err(); canceled != nil {
		return false, canceled
	}
	if err != nil {
		// Still fail closed for restricted credentials, while preserving the
		// transient cause so an existing registration can retry discovery.
		return false, errors.Join(errScopeUpgrade, err)
	}
	if !scoped {
		return false, errScopeUpgrade
	}
	return true, nil
}

// Three attempts at most, all with the same persisted request. No retry on
// credential, permission or identity rejection. A 429 can retry only after
// Retry-After; an excessive wait stops rather than polling early.
func (c *deviceAuthClient) exchange(ctx context.Context, r directCodeRequest) (directGrant, error) {
	if err := c.projectState.terminal(c.projectSource); err != nil {
		return directGrant{}, err
	}
	if r.Holder != nil {
		_, _, _ = c.discoverCapabilities(ctx)
	}
	form := url.Values{"client_id": {deviceClientID}, "harness": {r.Harness}, "session_id": {r.SessionID}, "asb_client_instance_id": {r.ClientID}, "request_id": {r.RequestID}}
	if r.Nickname != "" {
		form.Set("requested_nickname", r.Nickname)
	}
	if r.RegistrationID != "" {
		form.Set("registration_id", r.RegistrationID)
		form.Set("resume_secret", r.Secret)
	}
	if r.Holder != nil && c.holderSupported.Load() {
		raw, _ := json.Marshal(r.Holder)
		form.Set("holder", string(raw))
	}
	if r.HolderProof != nil && c.holderSupported.Load() {
		raw, _ := json.Marshal(r.HolderProof)
		form.Set("holder_proof", string(raw))
	}
	if r.Handover {
		form.Set("handover", "true")
		form.Set("pid", strconv.Itoa(r.PID))
		form.Set("cwd", r.CWD)
		if r.ProcessKey != "" {
			form.Set("process_key", r.ProcessKey)
		}
	}
	encoded := form.Encode()
	if len(encoded) > 16<<10 {
		return directGrant{}, errDirectFailed
	}
	attempts := 3
	if r.Recovery {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if err := c.policy.ensure(ctx); err != nil {
			if ctx.Err() != nil {
				return directGrant{}, ctx.Err()
			}
			var project *projectRefusalError
			var transientProject *directTransientError
			if errors.As(err, &project) || errors.As(err, &transientProject) {
				return directGrant{}, err
			}
			var temporary *httpPolicyTransientError
			if errors.As(err, &temporary) {
				return directGrant{}, &directTransientError{retryAfter: temporary.retryAfter}
			}
			if r.Recovery && !errors.Is(err, errDirectRate) {
				return directGrant{}, errHTTPTransportDenied
			}
			return directGrant{}, err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", c.origin+"/oauth/session_registration?project="+url.QueryEscape(c.project), strings.NewReader(encoded))
		if err != nil {
			return directGrant{}, errDirectFailed
		}
		req.Header.Set("Authorization", "Bearer "+c.projectToken)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		if hint := c.projectState.hint(c.projectSource); hint != "" {
			req.Header.Set("X-ASB-Project-Generation", hint)
		}
		if c.policy != nil {
			req.Header.Set("X-ASB-Client-Transport", "http")
		}
		res, err := c.http.Do(req)
		wait := time.Duration(attempt+1) * time.Second
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(res.Body, (64<<10)+1))
			res.Body.Close()
			if readErr == nil {
				if refusal := c.projectState.refusal(c.projectSource, res.StatusCode, data); refusal != nil {
					return directGrant{}, refusal
				}
			}
			if r.Recovery && res.StatusCode == 429 {
				delay, parseErr := recoveryRetryAfter(res.Header.Get("Retry-After"), responseRetryReference(res.Header, c.clock.now()))
				if parseErr != nil {
					return directGrant{}, errDirectRate
				}
				return directGrant{}, &directTransientError{retryAfter: delay}
			}
			if res.StatusCode == 429 {
				wait, err = directRetryAfter(res.Header.Get("Retry-After"), responseRetryReference(res.Header, c.clock.now()))
				if err != nil {
					return directGrant{}, errDirectRate
				}
			} else if readErr == nil && len(data) <= 64<<10 {
				var payload struct {
					directGrant
					Error           string                  `json:"error"`
					HandoverReason  string                  `json:"handover_reason"`
					HolderChallenge *broker.HolderChallenge `json:"holder_challenge"`
				}
				decoded := json.Unmarshal(data, &payload) == nil
				if res.StatusCode == 409 && decoded && (payload.HandoverReason == broker.CodeRegistrationInUse || payload.HandoverReason == broker.CodeNotFound || payload.HandoverReason == "owner-mismatch") {
					return directGrant{}, broker.HandoverRefusal{Reason: payload.HandoverReason, HolderChallenge: payload.HolderChallenge}
				}
				if r.Handover && res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != 429 {
					reason := payload.HandoverReason
					if res.StatusCode == 400 || res.StatusCode == 403 || res.StatusCode == 404 {
						reason = "unsupported-mode"
					}
					return directGrant{}, broker.HandoverRefusal{Reason: broker.SafeHandoverReason(reason), HolderChallenge: payload.HolderChallenge}
				}
				switch {
				case res.StatusCode == 200:
					if !decoded || !validDirectGrant(payload.directGrant) {
						return directGrant{}, errDirectFailed
					}
					return payload.directGrant, nil
				case res.StatusCode == 401:
					return directGrant{}, errProjectTokenRejected
				case res.StatusCode == 403 && decoded && payload.Error == "insufficient_scope":
					return directGrant{}, errInsufficientScope
				case res.StatusCode == 403 && isHTTPTransportDenied(data):
					return directGrant{}, errHTTPTransportDenied
				case res.StatusCode == 403:
					return directGrant{}, errDirectPolicy
				case res.StatusCode == 410 || (res.StatusCode == 404 && decoded && payload.Error == "project_not_found"):
					return directGrant{}, &directTransientError{}
				case res.StatusCode == 404 || res.StatusCode == 405:
					return directGrant{}, errDirectUnsupported
				case res.StatusCode == 409 && decoded && payload.Error == "capacity_exceeded":
					return directGrant{}, errDirectCapacity
				case res.StatusCode == 409:
					return directGrant{}, errDirectConflict
				case res.StatusCode < 500:
					return directGrant{}, errDirectFailed
				}
			} else if res.StatusCode < 500 { // A lost successful response is retryable.
				if res.StatusCode != 200 {
					return directGrant{}, errDirectFailed
				}
			}
		}
		if canceled := ctx.Err(); canceled != nil {
			return directGrant{}, canceled
		}
		if attempt == attempts-1 {
			if res != nil && res.StatusCode == 429 {
				return directGrant{}, &directTransientError{retryAfter: wait}
			}
			break
		}
		if err := c.clock.sleep(ctx, wait); err != nil {
			return directGrant{}, err
		}
	}
	return directGrant{}, &directTransientError{}
}
func directRetryAfter(raw string, now time.Time) (time.Duration, error) {
	if seconds, e := strconv.Atoi(raw); e == nil && seconds >= 0 && seconds <= 60 {
		return time.Duration(seconds) * time.Second, nil
	}
	if at, e := http.ParseTime(raw); e == nil {
		d := at.Sub(now)
		if d >= 0 && d <= time.Minute {
			return d, nil
		}
	}
	return 0, errDirectRate
}
func validDirectGrant(g directGrant) bool {
	if !strings.HasPrefix(g.AccessToken, sessionTokenPrefix) || g.TokenType != "Bearer" || g.ExpiresIn <= 0 || g.ExpiresIn > 30*24*60*60 || g.SessionGrantID == "" || g.Nickname == "" || len(g.Nickname) > 128 || g.Mode != "auto" || len(g.Scopes) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, s := range g.Scopes {
		switch s {
		// Historical scopes from older servers remain readable.
		case "session:read", "session:register", "session:rename", "session:manage", "session:spawn", "message:send", "message:receive", "memory:read", "memory:write":
		default:
			return false
		}
		if seen[s] {
			return false
		}
		seen[s] = true
	}
	return seen["session:register"]
}
