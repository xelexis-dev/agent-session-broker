package controlpanel

import (
	"crypto/rand"
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

var directUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var directClientID = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

type DirectRequest struct {
	Holder          *protocol.ReceiverHolder
	HolderProof     *protocol.HolderProof
	Handover        bool
	PID             int
	CWD, ProcessKey string
	DeviceRequest
	RequestedNickname string
	RegistrationID    string
	ResumeSecret      string // transient proof only; never persisted or logged
}
type DirectProof struct {
	HandoverFrom                                    *protocol.HandoverFrom
	PreviousIdentity                                string
	Absent                                          bool
	RegistrationID, OwnerID, Name, AdmissionGrantID string
	Revision                                        uint64
	Names                                           []string
}
type DirectResponse struct {
	HandoverFrom *protocol.HandoverFrom `json:"handover_from,omitempty"`
	DeviceTokenResponse
	Scopes         []string `json:"scopes"`
	AdmissionMode  string   `json:"admission_mode"`
	RegistrationID string   `json:"registration_id,omitempty"`
	Revision       uint64   `json:"revision,omitempty"`
}

func canonicalDirectIdentity(identity string) bool {
	var fields [3]string
	return json.Unmarshal([]byte(identity), &fields) == nil && (fields[0] == "claude" || fields[0] == "codex" || fields[0] == "kimi") && directUUID.MatchString(fields[1]) && directClientID.MatchString(fields[2])
}
func (r DirectRequest) valid() bool {
	return r.DeviceRequest.valid() && canonicalDirectIdentity(r.identity()) && utf8.ValidString(r.RequestedNickname) && len(r.RequestedNickname) <= 128 && len(r.RegistrationID) <= 128 && len(r.ResumeSecret) <= 256
}

// This preflight is read-only and runs before broker proof lookup. Exchange
// repeats every live check after the proof lookup, under the policy lock.
func (s *Store) directPreflight(project, raw string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.data.ScopeIssuanceReady {
		return errDirectUnavailable
	}
	_, _, err := s.directParentLocked(project, raw)
	return err
}

var (
	errDirectUnavailable = deviceError("direct_registration_unavailable")
	errDirectForbidden   = deviceError("insufficient_scope")
	errDirectConflict    = deviceError("identity_conflict")
	errDirectCapacity    = deviceError("capacity_exceeded")
	errDirectRate        = deviceError("rate_limited")
)

func (s *Store) directParentLocked(project, raw string) (Project, SessionToken, error) {
	p, parent, err := s.projectTokenLocked(project, raw)
	if err != nil {
		return p, parent, err
	}
	if !access.HasScope(parent.Scopes, access.SessionRegister) || !access.HasScope(parent.Scopes, access.SessionRegisterAuto) || parent.AdmissionMode != "auto" || s.projectRoleLocked(parent.OwnerID, project) != "owner" {
		return p, parent, errDirectForbidden
	}
	return p, parent, nil
}
func latestDirectOwner(s *Store, p Project, identity, excluding string) string {
	var newest time.Time
	owner := ""
	for _, t := range p.Tokens {
		if t.ID == excluding || t.BoundIdentity != identity || !s.sessionTokenEligibleLocked(p, t) || (t.AdmissionMode == "auto" && t.FirstConnectedAt.IsZero()) {
			continue
		}
		if !t.IssuedAt.Before(newest) {
			newest, owner = t.IssuedAt, t.OwnerID
		}
	}
	return owner
}
func (s *Store) ExchangeDirect(project, raw string, r DirectRequest, proof DirectProof, source string, plaintext bool) (DirectResponse, error) {
	if s.beforeTokenExchange != nil {
		s.beforeTokenExchange()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if plaintext && !s.data.AllowHTTP {
		return DirectResponse{}, errHTTPNotAllowed
	}
	if !s.data.ScopeIssuanceReady {
		return DirectResponse{}, errDirectUnavailable
	}
	if !r.valid() {
		return DirectResponse{}, errDeviceRequest
	}
	if s.closed || s.writeBatch == nil {
		return DirectResponse{}, ErrStorage
	}
	if err := s.expireLocked(); err != nil {
		return DirectResponse{}, err
	}
	p, parent, err := s.directParentLocked(project, raw)
	if err != nil {
		return DirectResponse{}, err
	}
	identity := r.identity()
	if r.Handover {
		if proof.RegistrationID == "" || proof.PreviousIdentity == "" {
			return DirectResponse{}, protocol.HandoverRefusal{Reason: "not-found"}
		}
		if proof.OwnerID != parent.OwnerID {
			return DirectResponse{}, protocol.HandoverRefusal{Reason: "owner-mismatch"}
		}
	}
	// A pending handover reserves its registration against an old receiver's
	// concurrent reauthorization until the rebind commits or the request expires.
	for _, b := range p.AutoBindings {
		if b.HandoverSource != "" && b.State != "confirmed" && b.ExpectedID == proof.RegistrationID && b.Identity != identity {
			return DirectResponse{}, errDirectConflict
		}
	}
	now := s.now().UTC()
	owner := latestDirectOwner(s, p, identity, "")
	if owner != "" && owner != parent.OwnerID {
		return DirectResponse{}, errDirectConflict
	}
	if proof.RegistrationID != "" {
		if proof.RegistrationID != r.RegistrationID || r.ResumeSecret == "" || (owner == "" && proof.OwnerID != parent.OwnerID) || proof.Revision == 0 {
			return DirectResponse{}, errDirectConflict
		}
		if r.RequestedNickname != "" && r.RequestedNickname != proof.Name && (!validNickname(r.RequestedNickname) || slices.Contains(proof.Names, r.RequestedNickname)) {
			return DirectResponse{}, errDirectConflict
		}
		if r.RequestedNickname != "" && r.RequestedNickname != proof.Name && !access.HasScope(parent.Scopes, access.SessionRename) {
			return DirectResponse{}, errDirectForbidden
		}
	} else if r.RegistrationID != "" {
		return DirectResponse{}, errDirectConflict
	}
	if protocol.IsAdminSender(r.RequestedNickname) || typedDirectName(r.RequestedNickname) {
		return DirectResponse{}, errDirectConflict
	}
	var cleanup []AuditEvent
	if proof.Absent {
		p.AutoBindings = slices.DeleteFunc(slices.Clone(p.AutoBindings), func(b AutoBinding) bool {
			if b.DetachPending && b.Identity == identity && b.OwnerID == parent.OwnerID {
				if !pendingDetachRevoked(p, b) {
					cleanup = append(cleanup, auditFor(project, b, b.OwnerID, "revoked", "explicit-release", now))
				}
				return true
			}
			return false
		})
	}
	var binding *AutoBinding
	var exchange *AutoExchange
	for i := range p.AutoBindings {
		if p.AutoBindings[i].ParentID == parent.ID && p.AutoBindings[i].Identity == identity {
			v := p.AutoBindings[i]
			binding = &v
			break
		}
	}
	for i := range p.AutoExchanges {
		e := p.AutoExchanges[i]
		if e.ParentID == parent.ID && e.RequestID == r.RequestID {
			if e.Identity != identity || e.RequestedName != r.RequestedNickname {
				return DirectResponse{}, errDirectConflict
			}
			exchange = &e
			break
		}
	}
	if exchange != nil {
		if binding == nil || binding.GrantID != exchange.GrantID || (binding.State == "confirmed" && !r.Handover) || (proof.AdmissionGrantID != "" && proof.AdmissionGrantID == binding.GrantID && !r.Handover) {
			return DirectResponse{}, errDirectConflict
		}
		if !(r.Handover && binding.HandoverSource != "" && proof.AdmissionGrantID == binding.GrantID && proof.Revision > binding.ExpectedRevision) {
			p, *binding, err = recoverDirectRename(p, *binding, proof)
		}
		if err != nil {
			return DirectResponse{}, err
		}
		if wait := s.directRateWaitLocked(project, parent, source, false); wait > 0 {
			return DirectResponse{}, &directRateError{wait: wait}
		}
		for i, t := range p.Tokens {
			if t.ID != exchange.GrantID {
				continue
			}
			if !s.sessionTokenEligibleLocked(p, t) || (!t.FirstConnectedAt.IsZero() && !r.Handover) {
				return DirectResponse{}, errDirectConflict
			}
			secret := "asb_s_" + rand.Text() + rand.Text()
			p.Tokens = slices.Clone(p.Tokens)
			p.Tokens[i].TokenHash = digest(secret)
			if err = s.commitDirectProject(p, []AuditEvent{auditFor(project, *binding, parent.OwnerID, "request", "replay", now)}); err != nil {
				return DirectResponse{}, err
			}
			return directResponse(secret, p.Tokens[i], *binding, now), nil
		}
		return DirectResponse{}, errDirectConflict
	}
	// A second request ID must prove any existing registration. It never adds a
	// second identity slot, and cannot silently rotate an active grant.
	if binding != nil && binding.State != "issued" && proof.RegistrationID == "" {
		return DirectResponse{}, errDirectConflict
	}
	if binding != nil && proof.RegistrationID == "" {
		return DirectResponse{}, errDirectConflict
	}
	count := 0
	for _, b := range p.AutoBindings {
		if b.ParentID == parent.ID && !(r.Handover && b.Identity == proof.PreviousIdentity) {
			count++
		}
	}
	if binding == nil && parent.MaxAutoBindings > 0 && count >= parent.MaxAutoBindings {
		return DirectResponse{}, errDirectCapacity
	}
	if len(p.AutoExchanges) >= 2048 || (binding == nil && len(p.AutoBindings) >= 1024) || len(s.audits)+2 > maxAuditEvents {
		return DirectResponse{}, errDirectCapacity
	}
	if activeTokenCount(p.Tokens, now) >= projectTokenCap {
		return DirectResponse{}, errDirectCapacity
	}
	if wait := s.directRateWaitLocked(project, parent, source, binding == nil && !r.Handover); wait > 0 {
		return DirectResponse{}, &directRateError{wait: wait}
	}
	name := proof.Name
	if name == "" {
		name = r.RequestedNickname
		occupied := func(name string) bool {
			if slices.Contains(proof.Names, name) {
				return true
			}
			for _, t := range p.Tokens {
				if tokenReservesName(t, name) {
					return true
				}
			}
			return false
		}
		if !validNickname(name) || occupied(name) {
			for {
				name = "auto-" + strings.ToLower(rand.Text())
				if validNickname(name) && !occupied(name) {
					break
				}
			}
		}
	}
	expires := now.Add(sessionLifetime)
	if !parent.ExpiresAt.IsZero() && parent.ExpiresAt.Before(expires) {
		expires = parent.ExpiresAt
	}
	secret := "asb_s_" + rand.Text() + rand.Text()
	id := rand.Text() + rand.Text()
	t := SessionToken{ScopeVersion: access.ScopeVersion, Scopes: access.DelegatedScopes(parent.Scopes), Kind: tokenSession, AdmissionMode: "auto", IssuedBy: parent.OwnerID, ParentID: parent.ID, ID: id, OwnerID: parent.OwnerID, Nickname: name, IssuedAt: now, ExpiresAt: expires, BoundIdentity: identity, TokenHash: digest(secret)}
	p.Tokens = slices.Clone(p.Tokens)
	if binding != nil {
		for i := range p.Tokens {
			if p.Tokens[i].ID == binding.GrantID {
				p.Tokens[i].Revoked = true
			}
		}
	}
	p.Tokens = append(p.Tokens, t)
	for i, v := range p.Tokens {
		if v.ID == parent.ID {
			p.Tokens[i].LastUsedAt = now
		}
	}
	b := AutoBinding{OwnerID: parent.OwnerID, ParentID: parent.ID, Identity: identity, GrantID: id, State: "issued", ExpiresAt: now.Add(directWindow), ExpectedID: proof.RegistrationID, ExpectedOwner: proof.OwnerID, ExpectedRevision: proof.Revision}
	if r.Handover {
		b.HandoverSource = proof.PreviousIdentity
	}
	if proof.RegistrationID != "" {
		b.State = "claiming"
		b.RegistrationID = proof.RegistrationID
	}
	p.AutoBindings = slices.Clone(p.AutoBindings)
	if binding == nil {
		p.AutoBindings = append(p.AutoBindings, b)
	} else {
		for i := range p.AutoBindings {
			if p.AutoBindings[i].ParentID == parent.ID && p.AutoBindings[i].Identity == identity {
				p.AutoBindings[i] = b
			}
		}
	}
	p.AutoExchanges = append(slices.Clone(p.AutoExchanges), AutoExchange{parent.ID, r.RequestID, identity, r.RequestedNickname, id, now.Add(directWindow)})
	events := append(cleanup, auditFor(project, b, parent.OwnerID, "request", "accepted", now), auditFor(project, b, parent.OwnerID, "grant", "issued", now))
	if err = s.commitDirectProject(p, events); err != nil {
		return DirectResponse{}, err
	}
	s.notifyTokenChangesLocked(project, p.Tokens)
	return directResponse(secret, t, b, now), nil
}
func directResponse(raw string, t SessionToken, b AutoBinding, now time.Time) DirectResponse {
	return DirectResponse{DeviceTokenResponse: DeviceTokenResponse{AccessToken: raw, TokenType: "Bearer", ExpiresIn: int(t.ExpiresAt.Sub(now).Seconds()), GrantID: t.ID, Nickname: t.Nickname}, Scopes: access.WireScopes(t.Scopes), AdmissionMode: "auto", RegistrationID: b.ExpectedID, Revision: b.ExpectedRevision}
}
func typedDirectName(name string) bool {
	for _, prefix := range []string{"registration:", "session:", "name:"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return registrationNickname.MatchString(name)
}
func (s *Store) commitDirectProject(p Project, events []AuditEvent) error {
	candidate := s.data
	candidate.Projects = maps.Clone(s.data.Projects)
	candidate.Projects[p.Name] = p
	return s.commitAudited(candidate, events)
}

// Authentication precedes disclosure of receiver metadata. The proof alone
// does not authorize a different token owner to probe a live holder.
func (s *Store) directHolderGrant(project, raw, owner string) (access.Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, parent, err := s.directParentLocked(project, raw)
	if err != nil {
		return access.Grant{}, err
	}
	if owner == "" || parent.OwnerID != owner {
		return access.Grant{}, errDirectForbidden
	}
	return access.Grant{Project: project, OwnerID: parent.OwnerID, Scopes: parent.Scopes}, nil
}
