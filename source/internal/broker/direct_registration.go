package broker

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type DirectProof struct {
	HandoverFrom                                    *protocol.HandoverFrom
	PreviousIdentity                                string
	Absent                                          bool
	RegistrationID, OwnerID, Name, AdmissionGrantID string
	Revision                                        uint64
	Names                                           []string
}

func (s *RemoteService) InspectDirectRegistration(ctx context.Context, harness, session, client, id, secret string) (DirectProof, error) {
	if ctx.Err() != nil {
		return DirectProof{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inspectDirectRegistrationLocked(harness, session, client, id, secret)
}

func (s *RemoteService) inspectDirectRegistrationLocked(harness, session, client, id, secret string) (DirectProof, error) {
	proof := DirectProof{}
	for _, e := range s.sessions {
		if e.record != nil || !e.retired {
			proof.Names = append(proof.Names, e.identity.Name)
		}
	}
	// A claimed registration removed by panel cleanup has no proof left to inspect.
	if id != "" && s.byRegistration[id] == nil {
		return DirectProof{}, protocol.HandoverRefusal{Reason: protocol.CodeNotFound}
	}
	e := s.durableIdentityLocked(harness, session, client)
	if e == nil {
		if id != "" {
			return DirectProof{}, errors.New("identity proof rejected")
		}
		proof.Absent = true
		return proof, nil
	}
	if id != e.record.RegistrationID || !secretMatches(e.record.SecretHash, secret) || e.record.Suspended || e.record.Detached {
		return DirectProof{}, errors.New("identity proof rejected")
	}
	proof.RegistrationID, proof.OwnerID, proof.Name, proof.Revision, proof.AdmissionGrantID = e.record.RegistrationID, e.record.OwnerID, e.record.Name, e.record.Revision, e.record.AdmissionGrantID
	return proof, nil
}
func (s *RemoteService) directFenceLocked(ctx context.Context, harness, session, client, id, name string, e *remoteSession) error {
	g, ok := access.FromContext(ctx)
	if !ok || g.Registration == nil {
		return nil
	}
	owner := ""
	revision := uint64(0)
	if e != nil {
		id, owner, revision = e.record.RegistrationID, e.record.OwnerID, e.record.Revision
	}
	raw, _ := json.Marshal([3]string{harness, session, client})
	return g.Registration(string(raw), id, owner, name, revision, false)
}
func (s *RemoteService) admissionMetadata(ctx context.Context, r *registrationRecord) {
	if g, ok := access.FromContext(ctx); ok {
		r.AdmissionParentID, r.AdmissionMode, r.AdmissionGrantID = g.ParentID, g.AdmissionMode, g.TokenID
	}
}
func (s *RemoteService) RegistrationAdmissionMatches(id, grant string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byRegistration[id]
	return e != nil && e.record.AdmissionGrantID == grant
}

// Direct recovery has a verified resume proof instead of an active receiver
// lease, but uses the exact existing atomic rename journal/persistence path.
func (s *RemoteService) RenameDirectRegistration(ctx context.Context, id, secret string, revision uint64, name string) (DirectProof, error) {
	if err := access.RequireScope(ctx, access.SessionRename); err != nil {
		return DirectProof{}, err
	}
	if !validDurableName(name) {
		return DirectProof{}, errors.New("invalid nickname")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byRegistration[id]
	if e == nil || !secretMatches(e.record.SecretHash, secret) || e.record.Revision != revision || e.record.Detached || e.record.Suspended {
		return DirectProof{}, errors.New("identity proof rejected")
	}
	if s.nameHolderBlocks(s.sessions[name], e) {
		return DirectProof{}, errors.New("nickname unavailable")
	}
	if err := s.authorizeLocked(ctx, e); err != nil {
		return DirectProof{}, err
	}
	if err := s.directFenceLocked(ctx, e.record.Harness, e.record.SessionID, e.record.ClientID, id, e.record.Name, e); err != nil {
		return DirectProof{}, err
	}
	if e.record.Name != name {
		if _, err := s.renameRecordLocked(ctx, e, name); err != nil {
			return DirectProof{}, err
		}
	}
	return DirectProof{RegistrationID: id, OwnerID: e.record.OwnerID, Name: e.record.Name, Revision: e.record.Revision, AdmissionGrantID: e.record.AdmissionGrantID}, nil
}

// WithDirectRegistration fences the final proof and policy exchange against retire.
// The callback runs under the broker lock and must not call back into the broker.
func (s *RemoteService) WithDirectRegistration(ctx context.Context, r RegisterRequest, id string, handover bool, apply func(DirectProof) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var proof DirectProof
	var err error
	if handover {
		var e *remoteSession
		e, err = s.handoverProofLocked(r, id)
		if err == nil {
			proof = DirectProof{HandoverFrom: e.record.HandoverFrom, RegistrationID: id, OwnerID: e.record.OwnerID, Name: e.record.Name, Revision: e.record.Revision, AdmissionGrantID: e.record.AdmissionGrantID, PreviousIdentity: bindingForRename(e)}
		}
	} else {
		proof, err = s.inspectDirectRegistrationLocked(r.Harness, r.SessionID, r.ClientID, id, r.Secret)
	}
	if err != nil {
		return err
	}
	return apply(proof)
}
