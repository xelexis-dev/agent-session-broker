package controlpanel

import (
	"slices"
	"sort"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
)

// These DTOs contain only panel-visible values, never stored identity records.
type projectDTO struct {
	Name       string    `json:"name"`
	Generation string    `json:"generation"`
	Mode       string    `json:"mode"`
	Enabled    bool      `json:"enabled"`
	Deleting   bool      `json:"deleting"`
	Role       string    `json:"role"`
	UpdatedAt  time.Time `json:"updated_at"`
	Version    string    `json:"version"`
}
type memberDTO struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Enabled bool   `json:"enabled"`
}
type projectTokenDTO struct {
	Version          string    `json:"version"`
	ID               string    `json:"id"`
	Label            string    `json:"label"`
	OwnerID          string    `json:"owner_id"`
	IssuedBy         string    `json:"issued_by"`
	Scopes           []string  `json:"scopes"`
	Admission        string    `json:"admission"`
	IssuedAt         time.Time `json:"issued_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	LastRegisteredAt time.Time `json:"last_registered_at"`
	Revoked          bool      `json:"revoked"`
	Approvals        int       `json:"approvals"`
	MaxAutoBindings  int       `json:"max_auto_bindings"`
}

func projectTokenProjection(p Project, t SessionToken) projectTokenDTO {
	return projectTokenDTO{Version: tokenScopeVersion(p.Name, p.ProjectGenerationID, t), ID: t.ID, Label: t.Label, OwnerID: t.OwnerID, IssuedBy: t.IssuedBy, Scopes: slices.Clone(t.Scopes), Admission: t.AdmissionMode, IssuedAt: t.IssuedAt, ExpiresAt: t.ExpiresAt, LastRegisteredAt: t.LastRegisteredAt, Revoked: t.Revoked, Approvals: t.Approvals, MaxAutoBindings: t.MaxAutoBindings}
}
func (s *Store) projectProjectionsFor(actor string) []projectDTO {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]projectDTO, 0)
	for _, p := range s.projectsForLocked(actor) {
		out = append(out, projectDTO{Name: p.Name, Generation: p.ProjectGenerationID, Mode: p.Mode, Enabled: p.Enabled, Deleting: p.Deleting, Role: s.projectRoleLocked(actor, p.Name), UpdatedAt: p.UpdatedAt, Version: projectSettingsVersion(p)})
	}
	return out
}
func (s *Store) memberProjectionsFor(actor, project string) ([]memberDTO, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.projectRoleLocked(actor, project) == "" {
		return nil, ErrForbidden
	}
	out := make([]memberDTO, 0)
	for id, role := range s.data.Projects[project].Members {
		out = append(out, memberDTO{ID: id, Role: role, Enabled: s.data.Users[id].Enabled})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s *Store) projectTokenProjectionsFor(actor, project string) ([]projectTokenDTO, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	role := s.projectRoleLocked(actor, project)
	if role == "" {
		return nil, ErrForbidden
	}
	out := make([]projectTokenDTO, 0)
	for _, t := range s.data.Projects[project].Tokens {
		if t.Kind == tokenProject && (role == "owner" || t.OwnerID == actor) {
			out = append(out, projectTokenProjection(s.data.Projects[project], t))
		}
	}
	return out, nil
}

type sessionRowDTO struct {
	ID             string          `json:"id"`
	RegistrationID string          `json:"registration_id"`
	GrantID        string          `json:"grant_id"`
	Name           string          `json:"name"`
	OwnerID        string          `json:"owner_id"`
	Harness        string          `json:"harness"`
	PID            int             `json:"pid"`
	ClientID       string          `json:"client_id"`
	Status         string          `json:"status"`
	Ready          bool            `json:"ready"`
	Suspended      bool            `json:"suspended"`
	CanManage      bool            `json:"can_manage"`
	LastSeen       string          `json:"last_seen"`
	Reason         string          `json:"reason"`
	RuntimeInfo    runtimeInfoView `json:"runtime_info"`
	Activity       activityView    `json:"activity"`
}

func (s *Store) sessionProjectionsFor(actor, project string, sessions []Session) ([]sessionRowDTO, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.data.Projects[project]
	if !ok || s.projectRoleLocked(actor, project) == "" {
		return nil, ErrForbidden
	}
	owners := s.sessionOwnersLocked(project, sessions)
	all := append(slices.Clone(sessions), reservedSessionRows(p, s.data.Users, sessions)...)
	out := make([]sessionRowDTO, 0, len(all))
	for _, item := range all {
		owner := owners[sessionIdentity(item)]
		manage := s.canManageSessionLocked(actor, project, sessionIdentity(item), item.OwnerID)
		if item.GrantID != "" {
			owner = item.GrantOwner
			manage = s.canReleaseReservedGrantLocked(actor, project, item.GrantID)
		}
		out = append(out, sessionRowDTO{ID: item.ID, RegistrationID: item.RegistrationID, GrantID: item.GrantID, Name: item.Name, OwnerID: owner, Harness: item.Harness, PID: item.PID, ClientID: item.ClientID, Status: item.Status, Ready: item.Ready, Suspended: item.Suspended, CanManage: manage, LastSeen: item.LastSeen, Reason: item.StateReason().In(i18n.English), RuntimeInfo: item.RuntimeInfoView(i18n.English), Activity: item.ActivityView(i18n.English)})
	}
	return out, nil
}

type projectAuditDTO struct {
	Project         string `json:"project"`
	GenerationLabel string `json:"generation_label"`
	At              string `json:"at"`
	Kind            string `json:"kind"`
	Event           string `json:"event"`
	Name            string `json:"name"`
	Registration    string `json:"registration"`
	Parent          string `json:"parent"`
	Peer            string `json:"peer"`
	Reason          string `json:"reason"`
	SessionURL      string `json:"session_url"`
}

// The panel's filtered row already strips binding hashes and downstream records.
func projectAuditProjection(r directAuditRow) projectAuditDTO {
	return projectAuditDTO{Project: r.Project, GenerationLabel: r.GenerationLabel, At: r.At, Kind: r.Kind, Event: r.Event, Name: r.Name, Registration: r.Registration, Parent: r.Parent, Peer: r.Peer, Reason: r.Reason, SessionURL: r.SessionURL}
}

type registrationAuditDTO projectAuditDTO

func registrationAuditProjection(r directAuditRow) registrationAuditDTO {
	return registrationAuditDTO(projectAuditProjection(r))
}

func (s *Store) projectAuditProjectionsFor(actor, project, bucket string) ([]projectAuditDTO, error) {
	view, err := s.directAuditViewBucketFor(actor, project, bucket, i18n.English)
	if err != nil {
		return nil, err
	}
	out := make([]projectAuditDTO, 0, len(view.Rows))
	for _, r := range view.Rows {
		out = append(out, projectAuditProjection(r))
	}
	return out, nil
}
