package clientstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type SavedConversation struct {
	Key   string
	State SessionState
}
type HandoverAttempt struct {
	Manual       bool         `json:"manual,omitempty"`
	StopReason   string       `json:"stopReason,omitempty"`
	SourceKey    string       `json:"sourceKey"`
	RequestID    string       `json:"requestId"`
	Registration Registration `json:"registration"`
}

// Only this client's own validated saved states are candidates. No broker list
// or arbitrary file path can provide a registration ownership proof.
func (s *Store) HandoverCandidates(scope, harness, client string) ([]SavedConversation, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "sessions"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("saved conversations unavailable")
	}
	var out []SavedConversation
	for _, entry := range entries {
		if !entry.IsDir() || !diagnosticKey.MatchString(entry.Name()) {
			continue
		}
		state, found, e := s.loadSession(entry.Name(), false)
		if e != nil || !found {
			continue
		}
		r := state.Registration
		if state.Scope != scope || state.Harness != harness || state.ClientID != client || r.RegistrationID == "" || r.HandedOverTo != "" {
			continue
		}
		out = append(out, SavedConversation{Key: entry.Name(), State: state})
	}
	return out, nil
}

// A separate handover marker cannot be erased by a late old client's
// credential/state write. LoadSession overlays it on the saved registration.
func (s *Store) MarkHandedOver(key, to string) error {
	if !diagnosticKey.MatchString(key) || !diagnosticKey.MatchString(to) || key == to {
		return errors.New("invalid handover state")
	}
	data, _ := json.Marshal(struct {
		To string    `json:"to"`
		At time.Time `json:"at"`
	}{to, time.Now().UTC()})
	return writeFileWith(filepath.Join(s.sessionDir(key), "handover.json"), data, replaceFile)
}
func (s *Store) handedOverTo(key string) string {
	data, err := os.ReadFile(filepath.Join(s.sessionDir(key), "handover.json"))
	if err != nil {
		return ""
	}
	var marker struct {
		To string `json:"to"`
	}
	if json.Unmarshal(data, &marker) != nil || !diagnosticKey.MatchString(marker.To) {
		return ""
	}
	return marker.To
}

var ErrHandoverChain = errors.New("handover chain is unavailable")
var ErrHandoverDetached = errors.New("handover conversation is detached")

// ErrHandoverNotFound is an explicit fail-closed resolution: no new admission
// may be attempted when saved revisions cannot identify one current holder.
var ErrHandoverNotFound = fmt.Errorf("%w: holder not found", ErrHandoverChain)

// HandoverHolder validates the bounded hint walk, then resolves ownership from
// saved revisions. A marker cannot erase the destination's committed credential
// or make an older source authoritative when either marker write was interrupted.
// Only an explicit handover may allow a detached registration to be revived.
func (s *Store) HandoverHolder(key, scope, harness, client string, allowDetached bool) (SavedConversation, error) {
	if !diagnosticKey.MatchString(key) {
		return SavedConversation{}, ErrHandoverNotFound
	}
	start, found, err := s.loadSession(key, false)
	if err != nil || !found || !savedHandoverIdentity(key, start, scope, harness, client) || start.Registration.RegistrationID == "" || start.Registration.Secret == "" {
		return SavedConversation{}, ErrHandoverNotFound
	}
	registration, secret := start.Registration.RegistrationID, start.Registration.Secret
	seen := map[string]bool{}
	for depth := 0; ; depth++ {
		if depth > 8 || !diagnosticKey.MatchString(key) || seen[key] {
			return SavedConversation{}, ErrHandoverChain
		}
		seen[key] = true
		state, found, e := s.loadSession(key, false)
		if e != nil || !found || !savedHandoverIdentity(key, state, scope, harness, client) {
			return SavedConversation{}, ErrHandoverChain
		}
		if state.Registration.RegistrationID != registration || state.Registration.Secret != secret {
			return SavedConversation{}, ErrHandoverNotFound
		}
		if state.Registration.Detached && !allowDetached {
			return SavedConversation{}, ErrHandoverDetached
		}
		if state.Registration.HandedOverTo == "" {
			break
		}
		key = state.Registration.HandedOverTo
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, "sessions"))
	if err != nil {
		return SavedConversation{}, ErrHandoverNotFound
	}
	var holder SavedConversation
	tied := false
	for _, entry := range entries {
		if !entry.IsDir() || !diagnosticKey.MatchString(entry.Name()) {
			continue
		}
		state, found, e := s.loadSession(entry.Name(), false)
		if e != nil || !found || !savedHandoverIdentity(entry.Name(), state, scope, harness, client) || state.Registration.RegistrationID != registration || state.Registration.Secret != secret {
			continue
		}
		if from := state.Registration.HandoverFrom; from != nil && from.Revision >= state.Registration.Revision {
			return SavedConversation{}, ErrHandoverNotFound
		}
		revision := state.Registration.Revision
		if holder.Key == "" || revision > holder.State.Registration.Revision {
			holder = SavedConversation{Key: entry.Name(), State: state}
			tied = false
		} else if revision == holder.State.Registration.Revision {
			tied = true
		}
	}
	if holder.Key == "" || tied {
		return SavedConversation{}, ErrHandoverNotFound
	}
	if holder.State.Registration.Detached && !allowDetached {
		return SavedConversation{}, ErrHandoverDetached
	}
	return holder, nil
}
func savedHandoverIdentity(key string, state SessionState, scope, harness, client string) bool {
	return state.Scope == scope && state.Harness == harness && state.ClientID == client && SessionKey(scope, harness, state.SessionID, client) == key
}

// Only a successful, proved handover may clear the destination's old marker.
// Ordinary credential writes cannot clear it because it remains a separate file.
func (s *Store) ClearHandedOver(key string) error {
	if !diagnosticKey.MatchString(key) {
		return errors.New("invalid handover state")
	}
	if err := os.Remove(filepath.Join(s.sessionDir(key), "handover.json")); err != nil && !os.IsNotExist(err) {
		return errors.New("handover state could not be cleared")
	}
	return nil
}
