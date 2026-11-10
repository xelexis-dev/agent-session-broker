package serverruntime

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"reflect"
	"testing"
)

func TestSessionTokenInfoUsesRestrictedDeviceGrant(t *testing.T) {
	r, parent := newDeviceRuntime(t)
	child := r.authorize(parent, protectedRequest(101, "ignored"), "scope-reader")
	scopes := []string{access.MessageSend, access.SessionRegister}
	restrictRuntimeChild(t, r, "scope-reader", scopes)
	info := managedTool[access.TokenInfo](t, r.bound(child), access.TokenInfoTool, struct{}{})
	if !reflect.DeepEqual(info.Scopes, scopes) || info.ExpiresAt.IsZero() {
		t.Fatal("credential info did not use authoritative restricted device grant")
	}
}
