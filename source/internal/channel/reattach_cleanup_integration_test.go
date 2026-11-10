package channel

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func cleanupPanelList(t *testing.T, r *detachedRuntime) string {
	t.Helper()
	u, err := url.Parse(r.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	origin := u.Scheme + "://" + u.Host
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(origin + "/login")
	if err != nil {
		t.Fatal("temporary login get")
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	token := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindSubmatch(raw)
	if len(token) != 2 {
		t.Fatal("temporary login csrf")
	}
	response, err = client.PostForm(origin+"/login", url.Values{"csrf": {string(token[1])}, "username": {"admin"}, "password": {"synthetic-detached-password"}})
	if err != nil {
		t.Fatal("temporary login post")
	}
	response.Body.Close()
	if response.StatusCode != 303 {
		t.Fatal("temporary admin login", response.StatusCode)
	}
	response, err = client.Get(origin + "/projects/sessions?project=detach")
	if err != nil {
		t.Fatal("temporary panel listing")
	}
	defer response.Body.Close()
	raw, _ = io.ReadAll(response.Body)
	if response.StatusCode != 200 {
		t.Fatal("temporary panel status", response.StatusCode)
	}
	return string(raw)
}

func TestReattachCleanupRealRuntimeOfflineStopListAndExplicitRecovery(t *testing.T) {
	r := newDetachedRuntime(t)
	manager := r.harness(t, t.TempDir(), 1701)
	manager.attach(t)
	dir := t.TempDir()
	first := r.harness(t, dir, 1702)
	first.attach(t)
	original, name := first.session.RegistrationID, first.session.Name
	first.close()
	if err := call(t.Context(), manager.client, "stop_session", map[string]string{"name": original}, nil); err != nil {
		t.Fatal(err)
	}
	if page := cleanupPanelList(t, r); strings.Contains(page, name) {
		t.Fatal("F1 left target row")
	}
	next := r.harness(t, dir, 1702)
	waitFor(t, "offline removed registration stops automatic recovery", func() bool { return strings.Contains(next.status(t).Message, "registration-not-found") })
	before := r.direct.Load()
	for i := 0; i < 3; i++ {
		_ = next.status(t)
		_, _ = next.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
	}
	if r.direct.Load() != before {
		t.Fatal("unprompted readmission after cleanup")
	}
	next.attach(t)
	if next.session.RegistrationID == original || next.session.Name != name {
		t.Fatal("explicit recovery did not create same-name replacement")
	}
	if r.direct.Load() != before+2 {
		t.Fatal("C1 did not use exactly N1 not-found and N2 fresh exchange")
	}
	if err := call(t.Context(), manager.client, "send_message", map[string]string{"from": manager.session.Name, "to": name, "body": "cleanup-survivor-delivery"}, nil); err != nil {
		t.Fatal("unrelated manager lost send permission")
	}
	select {
	case message := <-next.received:
		if message.Body != "cleanup-survivor-delivery" {
			t.Fatal("wrong recovered delivery")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recovered registration did not receive")
	}
}

func TestUnregFreshHarnessAndNewThreadsIssueNoGrant(t *testing.T) {
	r := newDetachedRuntime(t)
	for _, harness := range []string{"claude", "codex"} {
		dir := t.TempDir()
		for i := 1; i <= 50; i++ {
			id := singleIdentity()["11111111-1111-4111-8111-111111111111"]
			id.Harness = harness
			// Distinct thread IDs share one PC state directory, as /new and helpers do.
			id.SessionID = fmt.Sprintf("11111111-1111-4111-8111-%012d", 1800+i)
			h := r.identityHarness(t, dir, id)
			if h.status(t).Registered {
				t.Fatal("fresh conversation auto-registered")
			}
			_, _ = h.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
			h.close()
		}
	}
	if r.direct.Load() != 0 {
		t.Fatal("fresh MCP/helper issued an automatic grant")
	}
	manager := r.harness(t, t.TempDir(), 1901)
	manager.attach(t)
	before := r.direct.Load()
	dir := t.TempDir()
	original := r.harness(t, dir, 1902)
	original.attach(t)
	before++
	for i := 0; i < 10; i++ {
		h := r.harness(t, dir, 1910+i)
		_ = h.status(t)
		h.close()
	}
	if r.direct.Load() != before {
		t.Fatal("new thread issued grants without attach")
	}
}
