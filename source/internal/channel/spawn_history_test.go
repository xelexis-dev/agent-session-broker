package channel

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

func TestHistoricalSpawnScopesRemainReadable(t *testing.T) {
	scopes := []string{access.SessionRegister, access.SessionSpawn}
	raw, err := json.Marshal(scopes)
	if err != nil {
		t.Fatal("scope fixture")
	}
	if !slices.Equal(decodeCredentialScopes(raw), scopes) {
		t.Fatal("historical server observation rejected")
	}
	status := authScopes(context.Background(), nil, authStatus{Scopes: scopes})
	if status.ScopeSource != "stored" || !slices.Equal(status.Scopes, scopes) {
		t.Fatal("historical stored scopes rejected")
	}
	grant := directGrant{sessionGrant: sessionGrant{AccessToken: sessionTokenPrefix + "fixture", TokenType: "Bearer", ExpiresIn: 60, SessionGrantID: "grant", Nickname: "fixture"}, Mode: "auto", Scopes: scopes}
	if !validDirectGrant(grant) {
		t.Fatal("historical direct grant rejected")
	}
	grant.Scopes = []string{access.SessionSpawn}
	if validDirectGrant(grant) {
		t.Fatal("register requirement weakened")
	}
	if decodeCredentialScopes(json.RawMessage(`[]`)) != nil || decodeCredentialScopes(json.RawMessage(`["unknown"]`)) != nil {
		t.Fatal("invalid scopes became known permissions")
	}
}
