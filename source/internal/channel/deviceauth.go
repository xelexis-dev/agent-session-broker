package channel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Token kinds are distinguished by value prefix so the client never guesses
// a flow from a 401. Legacy session tokens have no prefix.
const (
	projectTokenPrefix  = "asb_p_"
	sessionTokenPrefix  = "asb_s_"
	deviceClientID      = "asb-client-v1"
	deviceScope         = "asb:session"
	deviceGrantType     = "urn:ietf:params:oauth:grant-type:device_code"
	defaultPollInterval = 5 * time.Second
	maxPollBackoff      = time.Minute
)

func isProjectToken(token string) bool { return strings.HasPrefix(token, projectTokenPrefix) }

var (
	errServerUpgradeRequired = errors.New("the broker does not offer device authorization; upgrade the server or use a session token")
	errProjectTokenRejected  = errors.New("the broker rejected the project token")
	errHTTPTransportDenied   = errors.New("HTTP transport is no longer allowed by the broker policy")
)

func isHTTPTransportDenied(body []byte) bool {
	var payload struct {
		Error string `json:"error"`
	}
	return json.Unmarshal(body, &payload) == nil && payload.Error == "http_not_allowed"
}

// deviceAuthClient performs RFC 8628 device authorization against the broker
// origin with the project token. The project token is sent only to the OAuth
// device/token/direct exchange endpoints, never to /mcp.
type deviceAuthClient struct {
	projectState    *projectTracker
	projectSource   string
	holderSupported atomic.Bool
	kimiSupported   atomic.Bool
	origin          string
	project         string
	projectToken    string
	http            *http.Client
	policy          *httpPolicyGuard
	clock           devicePollClock
}

// noRedirect makes an HTTP client return 3xx answers instead of following
// them. Go keeps Authorization across same-host redirects, so a redirected
// bearer could otherwise reach a downgraded, cross-origin or /mcp target.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func newDeviceAuthClient(endpoint, projectToken string, client *http.Client) *deviceAuthClient {
	u, _ := url.Parse(endpoint)
	project, _ := projectFromBrokerURL(endpoint)
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	// The project token goes to the configured origin's /oauth/* paths only;
	// an injected client gets the same policy without being mutated.
	bounded := *client
	bounded.CheckRedirect = noRedirect
	return &deviceAuthClient{origin: u.Scheme + "://" + u.Host, project: project, projectToken: projectToken, projectSource: credentialSource(projectToken), http: &bounded, policy: newHTTPPolicyGuard(endpoint, client), clock: realPollClock()}
}

var errDeviceRedirected = errors.New("the broker redirected the device authorization request; redirects are never followed")

type deviceCodeRequest struct {
	Harness, SessionID, ClientID, RequestID string
	CWDBasename                             string
}

type deviceAuthorization struct {
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type sessionGrant struct {
	AccessToken    string `json:"access_token"`
	TokenType      string `json:"token_type"`
	ExpiresIn      int    `json:"expires_in"`
	SessionGrantID string `json:"session_grant_id"`
	Nickname       string `json:"nickname"`
}

type tokenOutcome int

const (
	tokenGranted tokenOutcome = iota
	tokenPending
	tokenSlowDown
	tokenDenied
	tokenExpired
	tokenInvalidGrant
	tokenInvalidClient
	tokenServerError
	tokenTransportError
)

func (o tokenOutcome) final() bool {
	switch o {
	case tokenGranted, tokenDenied, tokenExpired, tokenInvalidGrant, tokenInvalidClient:
		return true
	}
	return false
}

func (c *deviceAuthClient) post(ctx context.Context, path string, form url.Values) (int, []byte, error) {
	if err := c.projectState.terminal(c.projectSource); err != nil {
		return 0, nil, err
	}
	if err := c.policy.ensure(ctx); err != nil {
		return 0, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+path+"?project="+url.QueryEscape(c.project), strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.projectToken)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if hint := c.projectState.hint(c.projectSource); hint != "" {
		request.Header.Set("X-ASB-Project-Generation", hint)
	}
	if c.policy != nil {
		request.Header.Set("X-ASB-Client-Transport", "http")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, errors.New("device authorization request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return 0, nil, errors.New("device authorization response could not be read")
	}
	if refusal := c.projectState.refusal(c.projectSource, response.StatusCode, body); refusal != nil {
		return response.StatusCode, body, refusal
	}
	return response.StatusCode, body, nil
}

func (c *deviceAuthClient) requestCode(ctx context.Context, r deviceCodeRequest) (deviceAuthorization, error) {
	form := url.Values{
		"client_id": {deviceClientID}, "scope": {deviceScope}, "harness": {r.Harness}, "session_id": {r.SessionID},
		"asb_client_instance_id": {r.ClientID}, "request_id": {r.RequestID}, "cwd_basename": {r.CWDBasename},
	}
	status, body, err := c.post(ctx, "/oauth/device_authorization", form)
	if err != nil {
		return deviceAuthorization{}, err
	}
	switch {
	case status >= 300 && status < 400:
		return deviceAuthorization{}, errDeviceRedirected
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		return deviceAuthorization{}, errServerUpgradeRequired
	case status == http.StatusForbidden && isHTTPTransportDenied(body):
		// The administrator may re-enable this policy without invalidating the
		// project token or this request's durable request ID.
		return deviceAuthorization{}, errHTTPTransportDenied
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return deviceAuthorization{}, errProjectTokenRejected
	case status != http.StatusOK:
		return deviceAuthorization{}, errors.New("device authorization was not issued")
	}
	var issued deviceAuthorization
	if json.Unmarshal(body, &issued) != nil || issued.DeviceCode == "" || issued.UserCode == "" || issued.ExpiresIn <= 0 {
		return deviceAuthorization{}, errors.New("device authorization response is invalid")
	}
	if issued.Interval <= 0 {
		issued.Interval = int(defaultPollInterval / time.Second)
	}
	resolved, err := c.resolveVerificationURI(issued.VerificationURI)
	if err != nil {
		return deviceAuthorization{}, err
	}
	issued.VerificationURI = resolved
	if issued.VerificationURIComplete != "" {
		complete, err := c.resolveVerificationURI(issued.VerificationURIComplete)
		if err != nil {
			return deviceAuthorization{}, err
		}
		issued.VerificationURIComplete = complete
	}
	return issued, nil
}

// resolveVerificationURI accepts a relative path or an absolute URL on the
// broker origin only. The user is never sent to a URL the server chose.
func (c *deviceAuthClient) resolveVerificationURI(raw string) (string, error) {
	base, _ := url.Parse(c.origin)
	if raw == "" {
		return "", errors.New("device authorization response has no verification_uri")
	}
	ref, err := url.Parse(raw)
	if err != nil || ref.User != nil {
		return "", errors.New("device authorization verification_uri is invalid")
	}
	resolved := base.ResolveReference(ref)
	if resolved.Scheme != base.Scheme || resolved.Host != base.Host {
		return "", errors.New("device authorization verification_uri is not on the broker origin")
	}
	return resolved.String(), nil
}

type redeemer interface {
	redeem(ctx context.Context, deviceCode string) (sessionGrant, tokenOutcome, error)
}

func (c *deviceAuthClient) redeem(ctx context.Context, deviceCode string) (sessionGrant, tokenOutcome, error) {
	form := url.Values{"client_id": {deviceClientID}, "grant_type": {deviceGrantType}, "device_code": {deviceCode}}
	status, body, err := c.post(ctx, "/oauth/token", form)
	if err != nil {
		if terminalProjectError(err) {
			return sessionGrant{}, tokenInvalidClient, err
		}
		return sessionGrant{}, tokenTransportError, err
	}
	var payload struct {
		sessionGrant
		Error string `json:"error"`
	}
	decoded := json.Unmarshal(body, &payload) == nil
	switch {
	case status == http.StatusOK:
		if !decoded || payload.AccessToken == "" {
			return sessionGrant{}, tokenServerError, errors.New("session credential response is invalid")
		}
		if payload.TokenType == "" {
			payload.TokenType = "Bearer"
		}
		return payload.sessionGrant, tokenGranted, nil
	case status >= 300 && status < 400:
		return sessionGrant{}, tokenServerError, errDeviceRedirected
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		return sessionGrant{}, tokenServerError, errServerUpgradeRequired
	case status == http.StatusForbidden && isHTTPTransportDenied(body):
		// A policy toggle races with an already-approved code. Treat it like a
		// transport interruption so polling can resume after re-enable.
		return sessionGrant{}, tokenTransportError, errHTTPTransportDenied
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return sessionGrant{}, tokenInvalidClient, errProjectTokenRejected
	case status == http.StatusBadRequest && decoded:
		switch payload.Error {
		case "authorization_pending":
			return sessionGrant{}, tokenPending, nil
		case "slow_down":
			return sessionGrant{}, tokenSlowDown, nil
		case "access_denied":
			return sessionGrant{}, tokenDenied, nil
		case "expired_token":
			return sessionGrant{}, tokenExpired, nil
		case "invalid_grant":
			return sessionGrant{}, tokenInvalidGrant, nil
		}
	}
	return sessionGrant{}, tokenServerError, errors.New("device token request failed")
}

type devicePollClock struct {
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

func realPollClock() devicePollClock {
	return devicePollClock{now: time.Now, sleep: func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}}
}

// pollDeviceToken polls the token endpoint until a final answer, the local
// expiry or cancellation. slow_down raises the interval by five seconds for
// the rest of the request (RFC 8628 §3.5); transport and server failures
// reduce frequency with a bounded backoff instead of ending the wait.
func pollDeviceToken(ctx context.Context, r redeemer, deviceCode string, interval time.Duration, expires time.Time, clock devicePollClock) (sessionGrant, tokenOutcome, error) {
	if interval <= 0 {
		interval = defaultPollInterval
	}
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return sessionGrant{}, tokenTransportError, err
		}
		if !clock.now().Before(expires) {
			return sessionGrant{}, tokenExpired, nil
		}
		grant, outcome, err := r.redeem(ctx, deviceCode)
		if ctx.Err() != nil {
			return sessionGrant{}, tokenTransportError, ctx.Err()
		}
		if outcome.final() {
			return grant, outcome, err
		}
		wait := interval
		switch outcome {
		case tokenSlowDown:
			interval += 5 * time.Second
			wait = interval
			failures = 0
		case tokenPending:
			failures = 0
		default:
			failures++
			wait = min(interval*time.Duration(1<<min(failures, 4)), maxPollBackoff)
		}
		if err := clock.sleep(ctx, wait); err != nil {
			return sessionGrant{}, tokenTransportError, err
		}
	}
}
