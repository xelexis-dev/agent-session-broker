package access

import (
	"context"
	"errors"
	"testing"
)

func TestRequireScope(t *testing.T) {
	if e := RequireScope(t.Context(), MemoryRead); e != nil {
		t.Fatal("local policy changed")
	}
	for _, scopes := range [][]string{nil, {SessionRegisterAuto}, {MemoryWrite}} {
		if e := RequireScope(WithGrant(t.Context(), Grant{Scopes: scopes}), MemoryRead); e != ErrInsufficientScope {
			t.Fatal("missing scope allowed")
		}
	}
	ctx := WithGrant(t.Context(), Grant{Scopes: LegacyV1Scopes()})
	for _, scope := range LegacyV1Scopes() {
		if e := RequireScope(ctx, scope); e != nil {
			t.Fatal("legacy permission denied")
		}
	}
	if e := RequireScope(ctx, ""); e != ErrInsufficientScope {
		t.Fatal("unclassified scope allowed")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if e := RequireScope(cancelled, MemoryRead); !errors.Is(e, context.Canceled) {
		t.Fatal("cancellation lost")
	}
}
