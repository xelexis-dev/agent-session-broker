package controlpanel

import (
	"encoding/json"
	"html"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestDeviceCompleteLinkPrefillAndNoGETApproval(t *testing.T) {
	f := newDeviceHTTPFixture(t)
	owner := loginPanelUser(t, f.h, "owner")
	token := issuedTokenFrom(t, owner.call("GET", issueProjectResult(t, owner, "one link"), nil))
	form := deviceForm("11111111-1111-4111-8111-111111111111", "link-req")
	form.Set("cwd_basename", "project<one>")
	_, issued := f.rest("POST", "/oauth/device_authorization?project=demo", token, form)
	code := issued["user_code"].(string)
	if issued["verification_uri_complete"] != "/auth/code?code="+url.QueryEscape(code) {
		t.Error("complete URI missing")
	}
	before, _ := json.Marshal(f.store.data)
	name := `worker<"&>`
	w := owner.call("GET", "/auth/code?"+url.Values{"code": {strings.ToLower(code)}, "name": {name}}.Encode(), nil)
	body := w.Body.String()
	for _, want := range []string{`value="` + html.EscapeString(name) + `"`, "pc-example", "claude", "project&lt;one&gt;", "11111111"} {
		if !strings.Contains(body, want) {
			t.Errorf("prefill/detail missing %s", want)
		}
	}
	if w.Code != 200 || !strings.Contains(body, `value="approve"`) {
		t.Error("GET did not show ready approval form")
	}
	after, _ := json.Marshal(f.store.data)
	if string(before) != string(after) {
		t.Error("GET changed control state")
	}
	// Even after the GET, a forged approve without CSRF must fail.
	req := httptest.NewRequest("POST", "http://broker/auth/code", strings.NewReader(url.Values{"action": {"approve"}, "code": {code}, "nickname": {name}}.Encode()))
	req.AddCookie(owner.cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	denied := httptest.NewRecorder()
	f.h.ServeHTTP(denied, req)
	if denied.Code != 403 {
		t.Error("approval without CSRF accepted")
	}
	if w = owner.call("POST", "/auth/code", url.Values{"action": {"approve"}, "code": {code}, "nickname": {name}}); w.Code != 303 {
		t.Errorf("one POST approval=%d", w.Code)
	}
}

func TestDeviceGETInvalidExpiredAndNickname(t *testing.T) {
	f := newDeviceHTTPFixture(t)
	owner := loginPanelUser(t, f.h, "owner")
	token := issuedTokenFrom(t, owner.call("GET", issueProjectResult(t, owner, "link invalid"), nil))
	_, issued := f.rest("POST", "/oauth/device_authorization?project=demo", token, deviceForm("11111111-1111-4111-8111-111111111111", "link-invalid"))
	code := issued["user_code"].(string)
	for _, bad := range []string{"BROKER-ADMIN", "name:worker", "has space"} {
		w := owner.call("GET", "/auth/code?"+url.Values{"code": {code}, "name": {bad}}.Encode(), nil)
		if strings.Contains(w.Body.String(), `name="nickname" value="`+bad+`"`) || !strings.Contains(w.Body.String(), "닉네임") {
			t.Error("invalid nickname prefilled")
		}
	}
	for _, invalid := range []string{"ZZZZ-ZZZZ", "<invalid>"} {
		w := owner.call("GET", "/auth/code?code="+url.QueryEscape(invalid), nil)
		if w.Code != 404 || !strings.Contains(w.Body.String(), "만료") {
			t.Error("invalid code not clearly marked")
		}
	}
	f.store.now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	w := owner.call("GET", "/auth/code?code="+url.QueryEscape(code), nil)
	if w.Code != 404 {
		t.Error("expired code accepted")
	}
}

func TestDeviceLinkLoginReturn(t *testing.T) {
	f := newDeviceHTTPFixture(t)
	target := "/auth/code?" + url.Values{"code": {"ABCD-EFGH"}, "name": {"worker<&>"}}.Encode()
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest("GET", "http://broker"+target, nil))
	redirect, e := url.Parse(w.Header().Get("Location"))
	if e != nil || redirect.Path != "/login" || redirect.Query().Get("next") != target {
		t.Error("login lost approval parameters")
	}
	if safeLoginNext(target) != target || safeLoginNext("//evil.example/auth/code?code=x") != "" {
		t.Error("unsafe login next")
	}
	login := httptest.NewRecorder()
	f.h.ServeHTTP(login, httptest.NewRequest("GET", "http://broker"+w.Header().Get("Location"), nil))
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(login.Body.String())
	if len(m) != 2 {
		t.Fatal("login CSRF missing")
	}
	user := &userPanelClient{t: t, h: f.h, cookie: login.Result().Cookies()[0], csrf: m[1]}
	result := user.call("POST", "/login", url.Values{"username": {"owner"}, "password": {testPassword}, "next": {target}})
	if result.Code != 303 || result.Header().Get("Location") != target {
		t.Error("login POST lost safe code/name target")
	}
}
