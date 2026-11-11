package controlpanel

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
)

// Device authorization REST for local clients (RFC 8628 profile). These paths
// take the project token as a bearer, never a browser session, and branch
// before the login redirect. Responses are JSON with no-store already set.
const (
	deviceAuthorizationPath = "/oauth/device_authorization"
	deviceTokenPath         = "/oauth/token"
	deviceVerificationPath  = "/auth/code"
	deviceGrantType         = "urn:ietf:params:oauth:grant-type:device_code"
)

func isDeviceRESTPath(path string) bool {
	return path == deviceAuthorizationPath || path == deviceTokenPath
}

func writeDeviceJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeDeviceError(w http.ResponseWriter, status int, code string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="asb-project"`)
	}
	writeDeviceJSON(w, status, map[string]string{"error": code})
}

func writeTransportDenied(w http.ResponseWriter) {
	writeDeviceJSON(w, http.StatusForbidden, map[string]string{"error": "http_not_allowed"})
}

func bearerToken(r *http.Request) string {
	if len(r.Header.Values("Authorization")) != 1 {
		return ""
	}
	scheme, value, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || len(value) > 1024 {
		return ""
	}
	return value
}

func (p *panel) deviceREST(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeDeviceError(w, http.StatusMethodNotAllowed, "invalid_request")
		return
	}
	if !p.store.Ready() {
		writeDeviceError(w, http.StatusServiceUnavailable, "server_error")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	projects := query["project"]
	if err != nil || len(projects) != 1 || !projectName.MatchString(projects[0]) {
		writeDeviceError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	project := projects[0]
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		writeDeviceError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	token := bearerToken(r)
	if token == "" {
		writeDeviceError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	// Check after reading the bounded request body so a policy toggle while a
	// client is uploading cannot admit a plaintext credential request.
	plaintext := p.plaintext(r)
	if plaintext && !p.store.AllowHTTP(project) {
		p.denyTransport(w, r, project)
		return
	}
	form := r.PostForm
	if r.URL.Path == deviceAuthorizationPath {
		if scope := form.Get("scope"); scope != "" && scope != "asb:session" {
			writeDeviceError(w, http.StatusBadRequest, "invalid_scope")
			return
		}
		request := DeviceRequest{CWDBasename: form.Get("cwd_basename"), ClientID: form.Get("client_id"), Harness: form.Get("harness"), SessionID: form.Get("session_id"), InstanceID: form.Get("asb_client_instance_id"), RequestID: form.Get("request_id")}
		issued, err := p.store.BeginDeviceAuthorization(project, token, request)
		if err != nil {
			p.writeDeviceFailure(w, err, true)
			return
		}
		writeDeviceJSON(w, http.StatusOK, issued)
		return
	}
	if form.Get("grant_type") != deviceGrantType {
		writeDeviceError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	granted, err := p.store.ExchangeDeviceToken(project, token, form.Get("device_code"), form.Get("client_id"), plaintext)
	if errors.Is(err, errHTTPNotAllowed) {
		p.denyTransport(w, r, project)
		return
	}
	if err != nil {
		p.writeDeviceFailure(w, err, false)
		return
	}
	writeDeviceJSON(w, http.StatusOK, granted)
}

func (p *panel) writeDeviceFailure(w http.ResponseWriter, err error, issuing bool) {
	var code deviceError
	switch {
	case errors.Is(err, errDeviceClient):
		writeDeviceError(w, http.StatusUnauthorized, string(errDeviceClient))
	case errors.As(err, &code):
		status := http.StatusBadRequest
		if issuing && code == errDeviceSlow {
			status = http.StatusTooManyRequests
		}
		writeDeviceError(w, status, string(code))
	default:
		writeDeviceError(w, http.StatusServiceUnavailable, "server_error")
	}
}

// allowCodeAttempt bounds user-code lookups per signed-in user so a short
// user code cannot be brute-forced through the approval form.
func (p *panel) allowCodeAttempt(userID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.codeAttempts == nil {
		p.codeAttempts = map[string]*attemptWindow{}
	}
	window := p.codeAttempts[userID]
	now := time.Now()
	if window == nil || now.Sub(window.started) >= time.Minute {
		window = &attemptWindow{started: now}
		p.codeAttempts[userID] = window
	}
	if len(p.codeAttempts) > 512 {
		for id, other := range p.codeAttempts {
			if id != userID && now.Sub(other.started) >= time.Minute {
				delete(p.codeAttempts, id)
			}
		}
	}
	if window.count >= 10 {
		return false
	}
	window.count++
	return true
}

type attemptWindow struct {
	started time.Time
	count   int
}

// authCode is the browser side of device authorization: a signed-in user
// enters the code shown by the client, reviews the self-reported identity and
// approves it with a nickname, or denies it.
func (p *panel) authCode(w http.ResponseWriter, r *http.Request, session loginSession) {
	user, ok := p.store.User(session.userID)
	if !ok || !user.Enabled {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	v := page{Lang: p.requestLanguage(r, user.ID), View: "authcode", CSRF: session.csrf, User: user, Form: r.PostForm, CanManage: map[string]bool{}}
	if v.Form == nil {
		v.Form = url.Values{}
	}
	if r.Method == http.MethodGet {
		v.Success = p.takeNotice(w, r)
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(query["code"]) > 1 || len(query["name"]) > 1 {
			v.Error = "device.error_link"
			p.render(w, r, 400, v)
			return
		}
		if _, present := query["code"]; !present {
			p.render(w, r, 200, v)
			return
		}
		code := normalizeUserCode(query.Get("code"))
		if len(code) == 8 {
			code = code[:4] + "-" + code[4:]
		}
		v.Code = code
		if !p.allowCodeAttempt(user.ID) {
			v.Error = "device.error_attempts"
			p.render(w, r, 429, v)
			return
		}
		if code == "" || len(code) > 32 {
			v.Error = "device.error_not_found"
			p.render(w, r, 404, v)
			return
		}
		view, lookupErr := p.store.DeviceAuthorizationFor(user.ID, code)
		if lookupErr != nil {
			v.Error = "device.error_not_found"
			p.render(w, r, 404, v)
			return
		}
		if view.Status != "pending" {
			v.Error = "device.error_used"
			p.render(w, r, 409, v)
			return
		}
		v.Device = &view
		if name := strings.TrimSpace(query.Get("name")); name != "" {
			if validNickname(name) {
				v.Form.Set("nickname", name)
			} else {
				v.Error = "device.error_link_nickname"
			}
		}
		p.render(w, r, 200, v)
		return
	}
	code := strings.TrimSpace(r.PostForm.Get("code"))
	action := r.PostForm.Get("action")
	v.Code = code
	if code == "" || len(code) > 32 {
		v.Error = "device.error_code_required"
		p.render(w, r, 400, v)
		return
	}
	view, decisionErr := p.decideDeviceCode(user.ID, code, action, r.PostForm.Get("nickname"), nil)
	if errors.Is(decisionErr, errDeviceCodeAttempts) {
		v.Error = "device.error_attempts"
		p.render(w, r, 429, v)
		return
	}
	fail := func(err error, status int, message string) {
		if errors.Is(err, ErrForbidden) {
			v.Error = "device.error_forbidden"
			p.render(w, r, 404, v)
			return
		}
		v.Error = message
		p.render(w, r, status, v)
	}
	switch action {
	case "approve":
		nickname := strings.TrimSpace(r.PostForm.Get("nickname"))
		v.Form.Set("nickname", nickname)
		view, err := view, decisionErr
		if err != nil {
			if current, lookupErr := p.store.DeviceAuthorizationFor(user.ID, code); lookupErr == nil {
				v.Device = &current
			}
			switch {
			case errors.Is(err, ErrTokenCap):
				fail(err, 409, tokenCapMessage)
			case errors.Is(err, ErrNicknameTaken):
				fail(err, 409, i18n.T(v.Lang, "device.error_nickname_taken", "nickname", nickname))
			case errors.Is(err, ErrInvalid):
				fail(err, 400, "device.error_nickname")
			case errors.Is(err, errDeviceGrant):
				fail(err, 409, "device.error_used")
			default:
				fail(err, 503, "device.error_approve_storage")
			}
			return
		}
		p.redirectNotice(w, r, projectURL("sessions", view.Project), "device-approved")
	case "deny":
		if err := decisionErr; err != nil {
			if errors.Is(err, errDeviceGrant) {
				fail(err, 409, "device.error_already_handled")
				return
			}
			fail(err, 503, "device.error_reject_storage")
			return
		}
		p.redirectNotice(w, r, deviceVerificationPath, "device-denied")
	default:
		view, err := view, decisionErr
		if err != nil {
			fail(err, 503, "device.error_lookup")
			return
		}
		v.Device = &view
		p.render(w, r, 200, v)
	}
}
