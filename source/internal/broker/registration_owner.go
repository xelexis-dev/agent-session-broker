package broker

import (
	"context"
	"encoding/json"
)

// Owner IDs stay on the server, separate from protocol.Session and client input.
func (s *RemoteService) registrationOwnerLocked(ctx context.Context) string {
	if admission, ok := s.admission.(interface{ RegistrationOwner(context.Context) string }); ok {
		return admission.RegistrationOwner(ctx)
	}
	return ""
}

// RegistrationOwners returns a detached snapshot keyed by registration ID.
func (s *RemoteService) RegistrationOwners() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	owners := map[string]string{}
	for id, e := range s.byRegistration {
		owners[id] = e.record.OwnerID
	}
	return owners
}

// BackfillRegistrationOwners only fills missing owners from retained server
// grants. Each write is atomic; a failed startup can safely retry the remainder.
func (s *RemoteService) BackfillRegistrationOwners(bindings map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.byRegistration {
		if e.record.OwnerID != "" {
			continue
		}
		identity, _ := json.Marshal([3]string{e.record.Harness, e.record.SessionID, e.record.ClientID})
		owner := bindings[string(identity)]
		if owner == "" {
			continue
		}
		next := *e.record
		next.OwnerID = owner
		if err := s.persistLocked(&next); err != nil {
			return err
		}
		*e.record = next
	}
	return nil
}

// PendingRegistrationOwnerBackfills counts eligible records, without exporting identities.
func (s *RemoteService) PendingRegistrationOwnerBackfills(bindings map[string]string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, e := range s.byRegistration {
		identity, _ := json.Marshal([3]string{e.record.Harness, e.record.SessionID, e.record.ClientID})
		if e.record.OwnerID == "" && bindings[string(identity)] != "" {
			count++
		}
	}
	return count
}
