package access

import (
	"slices"
	"testing"
)

func TestScopeAllowlistAndDelegation(t *testing.T) {
	legacy := LegacyV1Scopes()
	if len(legacy) != 8 || !ValidScopes(legacy) || HasScope(legacy, SessionRegisterAuto) || HasScope(nil, SessionRead) {
		t.Fatal("legacy scopes are not fixed/non-auto")
	}
	legacy[0] = "mutated"
	if !ValidScopes(LegacyV1Scopes()) {
		t.Fatal("legacy scope alias escaped")
	}
	for _, bad := range [][]string{nil, {}, {"*"}, {"future:permission"}, {SessionRead, SessionRead}, {SessionRegister, SessionRead}} {
		if ValidScopes(bad) {
			t.Fatal("noncanonical/unknown scopes accepted")
		}
	}
	parent := append(LegacyV1Scopes(), SessionRegisterAuto)
	slices.Sort(parent)
	child := DelegatedScopes(parent)
	if !ValidScopes(parent) || !ScopesSubset(child, parent) || HasScope(child, SessionRegisterAuto) || ScopesSubset(parent, child) {
		t.Fatal("child privilege escalation")
	}
	child[0] = "mutated"
	if !ValidScopes(parent) {
		t.Fatal("delegated snapshot aliases parent")
	}
}

func TestHistoricalSpawnReadOnly(t *testing.T) {
	historical := append(LegacyV1Scopes(), SessionSpawn)
	slices.Sort(historical)
	if !ValidScopes(historical) || IssuableScopes(historical) || !IssuableScopes(LegacyV1Scopes()) {
		t.Fatal("historical and issuable scopes must differ")
	}
	if HasScope(DelegatedScopes(historical), SessionSpawn) || HasScope(LegacyV1Scopes(), SessionSpawn) {
		t.Fatal("spawn must not be issued")
	}
	for _, item := range ScopeCatalog() {
		if item.Scope == SessionSpawn {
			t.Fatal("spawn still advertised")
		}
	}
}

func TestWireScopesKeepOldClientFullCount(t *testing.T) {
	stored := LegacyV1Scopes()
	full := WireScopes(stored)
	if len(full) != 9 || !ValidScopes(full) || !HasScope(full, SessionSpawn) || RestrictedScopes(stored) || RestrictedScopes(full) {
		t.Fatal("full grant must stay nine canonical scopes for 0.15-0.19 clients")
	}
	if HasScope(stored, SessionSpawn) || !slices.Equal(WireScopes(full), full) {
		t.Fatal("wire scopes mutated storage or duplicated compatibility scope")
	}
	full[0] = "mutated"
	if !ValidScopes(stored) {
		t.Fatal("wire scopes alias storage")
	}
	auto := WireScopes(append(LegacyV1Scopes(), SessionRegisterAuto))
	if len(auto) != 10 || !ValidScopes(auto) || RestrictedScopes(auto) {
		t.Fatal("project token wire scopes not canonical/full")
	}
	restricted := []string{MessageReceive, SessionRead, SessionRegister}
	if got := WireScopes(restricted); !slices.Equal(got, restricted) || !RestrictedScopes(got) {
		t.Fatal("restricted grant changed on the wire")
	}
	if WireScopes(nil) != nil || !RestrictedScopes(nil) {
		t.Fatal("missing scopes gained permissions")
	}
}
