package controlpanel

import (
	"crypto/rand"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

const directWindow = 10 * time.Minute
const auditRetention = 90 * 24 * time.Hour
const maxAuditEvents = 32768

type AutoBinding struct {
	HandoverSource  string    `json:"handoverSource,omitempty"`
	DetachPending   bool      `json:"detachPending,omitempty"`
	OwnerID         string    `json:"ownerID"`
	PendingRevision uint64    `json:"pendingRevision,omitempty"`
	ParentID        string    `json:"parentID"`
	Identity        string    `json:"identity"`
	GrantID         string    `json:"grantID"`
	RegistrationID  string    `json:"registrationID,omitempty"`
	State           string    `json:"state"`
	ExpiresAt       time.Time `json:"expiresAt"`
	// Proof is server-verified, not a client assertion. No secret is stored.
	ExpectedID       string `json:"expectedID,omitempty"`
	ExpectedOwner    string `json:"expectedOwner,omitempty"`
	ExpectedRevision uint64 `json:"expectedRevision,omitempty"`
}
type AutoExchange struct {
	ParentID      string    `json:"parentID"`
	RequestID     string    `json:"requestID"`
	Identity      string    `json:"identity"`
	RequestedName string    `json:"requestedName,omitempty"`
	GrantID       string    `json:"grantID"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

type AuditEvent struct {
	TokenScopes         *TokenScopeAudit          `json:"tokenScopes,omitempty"`
	ProjectGenerationID string                    `json:"projectGenerationId,omitempty"`
	Handover            *protocol.HandoverAudit   `json:"handover,omitempty"`
	Management          *protocol.ManagementEvent `json:"management,omitempty"`
	Version             int                       `json:"version"`
	Project             string                    `json:"project"`
	ParentID            string                    `json:"parentID"`
	GrantID             string                    `json:"grantID,omitempty"`
	Binding             string                    `json:"binding"` // one-way internal reference, never the raw identity
	OwnerID             string                    `json:"ownerID"`
	Kind                string                    `json:"kind"`
	Reason              string                    `json:"reason"`
	At                  time.Time                 `json:"at"`
}

func auditFor(project string, b AutoBinding, owner, kind, reason string, now time.Time) AuditEvent {
	return AuditEvent{Version: 1, Project: project, ParentID: b.ParentID, GrantID: b.GrantID, Binding: digest(b.Identity), OwnerID: owner, Kind: kind, Reason: reason, At: now.UTC()}
}
func validAudit(e AuditEvent) bool {
	if e.TokenScopes != nil || e.Kind == tokenScopesAuditKind {
		return validTokenScopeAudit(e)
	}
	if e.ProjectGenerationID != "" && !registrationNickname.MatchString(e.ProjectGenerationID) {
		return false
	}
	if h := e.Handover; h != nil {
		return e.Version == 1 && projectName.MatchString(e.Project) && e.OwnerID != "" && e.Kind == "handover" && h.RegistrationID != "" && h.Name != "" && len(h.Previous) == 64 && len(h.Current) == 64 && !h.At.IsZero() && h.At.Equal(e.At)
	}
	if e.Management != nil {
		m := e.Management
		return e.Version == 1 && projectName.MatchString(e.Project) && e.OwnerID != "" && e.ParentID == "" && e.GrantID == "" && e.Binding == "" && e.Reason == "" && (e.Kind == "detached" || e.Kind == "suspended" || e.Kind == "unsuspended") && m.Kind == e.Kind && m.At.Equal(e.At) && !m.At.IsZero() && m.ActorName != "" && m.TargetName != "" && m.TargetRegistrationID != ""
	}

	if e.Version != 1 || !projectName.MatchString(e.Project) || len(e.ParentID) != 52 || len(e.Binding) != 64 || e.OwnerID == "" || e.At.IsZero() {
		return false
	}
	switch e.Kind {
	case "request", "grant", "registration", "rejected", "revoked", "expired":
	default:
		return false
	}
	switch e.Reason {
	case "superseded", "accepted", "issued", "confirmed", "replay", "parent-inactive", "reservation-expired", "explicit-release", "forbidden", "capacity", "conflict", "rate-limited":
	default:
		return false
	}
	return e.GrantID == "" || len(e.GrantID) == 52
}
func (s *Store) writeAuditBatch(data []byte, events []AuditEvent, projects map[string]Project, apiEvents ...*apiAuditEvent) error {
	if s.writeBatch == nil {
		return ErrStorage
	}
	next := maps.Clone(s.audits)
	if next == nil {
		next = map[string]AuditEvent{}
	}
	puts := map[string][]byte{"state": data}
	var deletes []string
	now := s.now()
	for key, e := range next {
		if pruneGenerationAudit(e, s.data.Projects, projects) || !now.Before(e.At.Add(auditRetention)) {
			delete(next, key)
			deletes = append(deletes, key)
		}
	}
	if len(next)+len(events) > maxAuditEvents {
		return ErrStorage
	}
	for _, e := range events {
		p, ok := projects[e.Project]
		if !ok || p.ProjectGenerationID == "" && s.data.Version >= projectGenerationVersion {
			return ErrStorage
		}
		if e.ProjectGenerationID != "" && e.ProjectGenerationID != p.ProjectGenerationID {
			return ErrForbidden
		}
		e.ProjectGenerationID = p.ProjectGenerationID
		if !validAudit(e) {
			return ErrStorage
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return ErrStorage
		}
		key := "audit/" + rand.Text()
		for {
			if _, exists := next[key]; !exists {
				break
			}
			key = "audit/" + rand.Text()
		}
		puts[key] = raw
		next[key] = e
	}
	var err error
	if len(apiEvents) > 0 && apiEvents[0] != nil {
		err = s.apiBatchLocked(puts, deletes, *apiEvents[0])
	} else {
		err = s.writeBatch(puts, deletes)
	}
	if err != nil {
		return err
	}
	s.audits = next
	return nil
}
func (s *Store) loadAuditRecords(db *sqlitestore.Store) error {
	records, err := db.List()
	if err != nil {
		return err
	}
	for key, raw := range records {
		if !strings.HasPrefix(key, "audit/") {
			continue
		}
		if len(raw) > 4096 || len(s.audits) >= maxAuditEvents {
			return ErrStorage
		}
		var e AuditEvent
		if json.Unmarshal(raw, &e) != nil || !validAudit(e) {
			return ErrStorage
		}
		s.audits[key] = e
	}
	return nil
}

// Audit is not bearer-readable and preserves the existing panel role boundary.
func (s *Store) DirectAuditFor(actor, project string) ([]AuditEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.directAuditForLocked(actor, project)
}
func (s *Store) directAuditForLocked(actor, project string) ([]AuditEvent, error) {
	return s.directAuditRecordsLocked(actor, project, AuditFilter{})
}
func sortAuditRecords(out []AuditEvent) {
	slices.SortFunc(out, func(a, b AuditEvent) int { return a.At.Compare(b.At) })
}

func validAutoState(st state) bool {
	for _, p := range st.Projects {
		if len(p.AutoBindings) > 1024 || len(p.AutoExchanges) > 2048 {
			return false
		}
		if st.Version < scopeIntroductionVersion && (len(p.AutoBindings) > 0 || len(p.AutoExchanges) > 0) {
			return false
		}
		seen := map[string]bool{}
		for _, b := range p.AutoBindings {
			if b.HandoverSource != "" && !canonicalDirectIdentity(b.HandoverSource) {
				return false
			}
			key := b.ParentID + "/" + b.Identity
			if b.OwnerID == "" || seen[key] || len(b.ParentID) != 52 || len(b.GrantID) != 52 || !canonicalDirectIdentity(b.Identity) || b.ExpiresAt.IsZero() {
				return false
			}
			seen[key] = true
			switch b.State {
			case "issued", "claiming", "confirmed":
			default:
				return false
			}
			if b.State != "issued" && b.RegistrationID == "" {
				return false
			}
			if b.ExpectedID == "" && (b.ExpectedOwner != "" || b.ExpectedRevision != 0) {
				return false
			}
			if b.ExpectedID != "" && b.ExpectedRevision == 0 {
				return false
			}
		}
		seen = map[string]bool{}
		for _, e := range p.AutoExchanges {
			key := e.ParentID + "/" + e.RequestID
			if seen[key] || len(e.ParentID) != 52 || len(e.GrantID) != 52 || !canonicalDirectIdentity(e.Identity) || !safeDeviceText(e.RequestID, 128) || len(e.RequestedName) > 128 || e.ExpiresAt.IsZero() {
				return false
			}
			seen[key] = true
		}
	}
	return true
}

// Preserve claiming/confirmed allocations even when token history is swept.
// Only never-claimed reservations can age out without checking the broker.
func (s *Store) prepareAutoCommit(candidate state, events []AuditEvent) (state, []AuditEvent) {
	candidate.Projects = maps.Clone(candidate.Projects)
	now := s.now()
	for name, p := range candidate.Projects {

		p.Tokens = slices.Clone(p.Tokens)
		inactive := map[string]bool{}
		for _, t := range p.Tokens {
			if t.Kind == tokenProject && t.AdmissionMode == "auto" && !tokenActive(t, now) {
				inactive[t.ID] = true
			}
		}
		for i, t := range p.Tokens {
			if t.Kind == tokenSession && inactive[t.ParentID] && !t.Revoked {
				p.Tokens[i].Revoked = true
				for _, b := range p.AutoBindings {
					if b.GrantID == t.ID {
						events = append(events, auditFor(name, b, b.OwnerID, "revoked", "parent-inactive", now))
						break
					}
				}
			}
		}
		p.AutoBindings = slices.Clone(p.AutoBindings)
		var bindings []AutoBinding
		for _, b := range p.AutoBindings {
			owner := b.OwnerID
			parentActive := false
			for _, t := range p.Tokens {
				if t.ID == b.ParentID {
					owner = t.OwnerID
					parentActive = tokenActive(t, now)
					break
				}
			}
			drop := b.State == "issued" && !now.Before(b.ExpiresAt)
			for i, t := range p.Tokens {
				if t.ID != b.GrantID || t.Revoked {
					continue
				}
				if !parentActive || drop {
					p.Tokens[i].Revoked = true
					reason, kind := "parent-inactive", "revoked"
					if drop {
						reason, kind = "reservation-expired", "expired"
					}
					if owner != "" {
						events = append(events, auditFor(name, b, owner, kind, reason, now))
					}
				}
			}
			if !drop {
				bindings = append(bindings, b)
			}
		}
		p.AutoBindings = bindings
		p.AutoExchanges = slices.DeleteFunc(slices.Clone(p.AutoExchanges), func(e AutoExchange) bool { return !now.Before(e.ExpiresAt) })
		candidate.Projects[name] = p
	}
	return candidate, events
}

func (s *Store) pruneDirectAuditLocked() error {
	for _, e := range s.audits {
		if !s.now().Before(e.At.Add(auditRetention)) {
			raw, err := json.Marshal(s.data)
			if err != nil {
				return ErrStorage
			}
			return s.writeAuditBatch(raw, nil, s.data.Projects)
		}
	}
	return nil
}

// Only authenticated parent failures are audited; arbitrary bearer guesses
// cannot create durable records. Denial writes are themselves rate bounded.
func (s *Store) recordDirectDenial(project, raw string, r DirectRequest, source string, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !bool(s.data.ScopeIssuanceReady) || s.writeBatch == nil || !r.valid() {
		return
	}
	_, parent, err := s.projectTokenLocked(project, raw)
	if err != nil {
		return
	}
	if s.directRateWaitLocked(project, parent, "denied/"+source, false) > 0 {
		return
	}
	b := AutoBinding{ParentID: parent.ID, Identity: r.identity()}
	_ = s.commitAudited(s.data, []AuditEvent{auditFor(project, b, parent.OwnerID, "rejected", reason, s.now())})
}

// Ordinary revoke APIs may already have marked children revoked before this
// commit. Compare published state too, so those paths cannot skip direct audit.
func (s *Store) auditAutoTransitions(candidate state, events []AuditEvent) []AuditEvent {
	for project, old := range s.data.Projects {
		if len(old.AutoBindings) == 0 {
			continue
		}
		current, ok := candidate.Projects[project]
		if !ok {
			continue
		}
		before, after := map[string]SessionToken{}, map[string]SessionToken{}
		for _, t := range old.Tokens {
			before[t.ID] = t
		}
		for _, t := range current.Tokens {
			after[t.ID] = t
		}
		for _, b := range old.AutoBindings {
			previous, exists := before[b.GrantID]
			if !exists || previous.Revoked {
				continue
			}
			next, present := after[b.GrantID]
			if !present || !next.Revoked {
				continue
			}
			recorded := false
			for _, e := range events {
				if e.Project == project && e.GrantID == b.GrantID && (e.Kind == "revoked" || e.Kind == "expired") {
					recorded = true
					break
				}
			}
			if recorded {
				continue
			}
			reason := "superseded"
			if parent, exists := after[b.ParentID]; !exists || !tokenActive(parent, s.now()) {
				reason = "parent-inactive"
			}
			events = append(events, auditFor(project, b, b.OwnerID, "revoked", reason, s.now()))
		}
	}
	return events
}
