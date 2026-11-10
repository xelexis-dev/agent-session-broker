package channel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

// httpPolicyGuard verifies the explicit per-project opt-in before an official
// client sends a project or prefixed session bearer over plain HTTP. It is an
// opt-in check, not evidence of the remote server's identity.
type httpPolicyGuard struct {
	projectState  *projectTracker
	projectSource string
	origin        string
	project       string
	client        *http.Client
}

func newHTTPPolicyGuard(endpoint string, client *http.Client) *httpPolicyGuard {
	u, err := url.Parse(endpoint)
	project, ok := projectFromBrokerURL(endpoint)
	if err != nil || !ok || u.Scheme != "http" {
		return nil
	}
	if client == nil {
		client = &http.Client{}
	}
	// Do not mutate an injected client and do not allow its cookie jar to send
	// credentials on the deliberately unauthenticated policy request.
	bounded := *client
	bounded.Jar = nil
	bounded.CheckRedirect = noRedirect
	return &httpPolicyGuard{origin: u.Scheme + "://" + u.Host, project: project, client: &bounded}
}

type httpPolicyTransientError struct {
	message    string
	retryAfter time.Duration
}

func (e *httpPolicyTransientError) Error() string { return e.message }

func (g *httpPolicyGuard) ensure(ctx context.Context) error {
	if g == nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.origin+"/oauth/transport-policy?"+url.Values{"project": {g.project}}.Encode(), nil)
	if err != nil {
		return errors.New("HTTP transport policy request could not be created")
	}
	req.Header.Set("Accept", "application/json")
	if hint := g.projectState.hint(g.projectSource); hint != "" {
		req.Header.Set("X-ASB-Project-Generation", hint)
	}
	res, err := g.client.Do(req)
	if err != nil {
		return &httpPolicyTransientError{message: "HTTP transport policy could not be checked"}
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusMultipleChoices && res.StatusCode < http.StatusBadRequest {
		return errors.New("HTTP transport policy request was redirected")
	}
	if res.StatusCode >= 500 {
		return &httpPolicyTransientError{message: "HTTP transport policy is temporarily unavailable"}
	}
	if res.StatusCode == 429 {
		delay, err := recoveryRetryAfter(res.Header.Get("Retry-After"), responseRetryReference(res.Header, time.Now()))
		if err != nil {
			return errDirectRate
		}
		return &httpPolicyTransientError{message: "HTTP transport policy is rate limited", retryAfter: delay}
	}
	if res.StatusCode >= 400 && res.StatusCode < 500 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		if refusal := g.projectState.refusal(g.projectSource, res.StatusCode, body); refusal != nil {
			return refusal
		}
	}
	if res.StatusCode != http.StatusOK {
		return errors.New("HTTP transport is not allowed by the broker policy")
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return &httpPolicyTransientError{message: "HTTP transport policy response could not be read"}
	}
	var policy struct {
		Project   string `json:"project"`
		AllowHTTP bool   `json:"allow_http"`
	}
	if json.Unmarshal(body, &policy) != nil || policy.Project != g.project || !policy.AllowHTTP {
		return errors.New("HTTP transport is not allowed by the broker policy")
	}
	return nil
}
