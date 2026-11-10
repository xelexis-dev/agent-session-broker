package channel

import (
	"context"
	"slices"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

// Auth status is diagnostic: one bounded read, never a retry or authorization flow.
func authScopes(ctx context.Context, upstream Upstream, out authStatus) authStatus {
	out.ScopeSource = "unknown"
	if len(out.Scopes) > 0 {
		stored := slices.Clone(out.Scopes)
		slices.Sort(stored)
		if access.ValidScopes(stored) {
			out.Scopes = stored
			out.ScopeSource = "stored"
			out.ScopeDetails = access.ScopeDetails(stored)
		} else {
			out.Scopes = nil
			out.AdmissionMode = ""
		}
	}
	if upstream == nil || out.Status != authAuthorized {
		return out
	}
	if !currentReceiveScopeOwner(upstream) {
		out.Scopes = nil
		out.ScopeDetails = nil
		out.ScopeSource = "unknown"
		return out
	}

	if scopes := observedCredentialScopes(upstream); scopes != nil {
		out.Scopes = scopes
		out.ScopeSource = "server"
		out.ScopeDetails = access.ScopeDetails(scopes)
		return out
	}

	query, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var info access.TokenInfo
	if err := call(query, upstream, access.TokenInfoTool, struct{}{}, &info); err != nil {
		return out
	}
	if !currentReceiveScopeOwner(upstream) {
		out.Scopes = nil
		out.ScopeDetails = nil
		out.ScopeSource = "unknown"
		return out
	}

	if scopes := observedCredentialScopes(upstream); scopes != nil {
		out.Scopes = scopes
		out.ScopeSource = "server"
		out.ScopeDetails = access.ScopeDetails(scopes)
		return out
	}
	scopes := slices.Clone(info.Scopes)
	slices.Sort(scopes)
	if !access.ValidScopes(scopes) || (info.AdmissionMode != "" && info.AdmissionMode != "auto" && info.AdmissionMode != "manual") {
		return out
	}
	out.Scopes = scopes
	out.AdmissionMode = info.AdmissionMode
	out.ScopeSource = "server"
	out.ScopeDetails = access.ScopeDetails(scopes)
	out.CredentialExpiresAt = ""
	if !info.ExpiresAt.IsZero() {
		out.CredentialExpiresAt = info.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return out
}

func observedCredentialScopes(upstream Upstream) []string {
	if wrapped, ok := upstream.(*receiveUpstream); ok {
		wrapped.currentPermission()
		wrapped.registry.mu.Lock()
		current := wrapped.registry.entries[wrapped.key] == wrapped.permission
		wrapped.registry.mu.Unlock()
		if !current {
			return nil
		}
		upstream = wrapped.Upstream
	}
	if observer, ok := upstream.(interface{ receiveScopeObservation() (uint64, []string) }); ok {
		_, scopes := observer.receiveScopeObservation()
		return scopes
	}
	return nil
}

func currentReceiveScopeOwner(upstream Upstream) bool {
	wrapped, ok := upstream.(*receiveUpstream)
	if !ok {
		return true
	}
	wrapped.registry.mu.Lock()
	defer wrapped.registry.mu.Unlock()
	return wrapped.registry.entries[wrapped.key] == wrapped.permission
}
