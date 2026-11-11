package controlpanel

import (
	"cmp"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
)

const directAuditLimit = 100

type directAuditRow struct {
	At, AtLabel, Kind, Event, Name, Registration, Parent, Peer, Reason, SessionURL string
	Project, GenerationLabel                                                       string
}
type directAuditView struct {
	Enabled, Limited                              bool
	Rows                                          []directAuditRow
	History                                       bool
	Notice, CurrentURL, HistoryURL, AllHistoryURL string
}

// Projection is built under the same policy lock as filtering. It never returns
// AuditEvent's binding digest, token identifiers, credential hashes or bodies.
// Labels are in language: the page's language for the panel, English for the API.
func (s *Store) directAuditViewFor(actor, project, language string) (directAuditView, error) {
	return s.directAuditViewBucketFor(actor, project, "current", language)
}

func (s *Store) directAuditViewBucketFor(actor, project, bucket, language string) (directAuditView, error) {
	return s.directAuditViewWindowFor(actor, project, bucket, language, time.Time{}, directAuditLimit)
}
func (s *Store) directAuditViewWindowFor(actor, project, bucket, language string, before time.Time, limit int) (directAuditView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	events, err := s.directAuditRecordsLocked(actor, project, AuditFilter{Bucket: bucket})
	history := bucket != "current"
	v := directAuditView{Enabled: history || bool(s.data.ScopeIssuanceReady), History: history, CurrentURL: auditHistoryURL(project, "current"), HistoryURL: auditHistoryURL(project, "history"), AllHistoryURL: auditHistoryURL("", "history")}
	if project == "" {
		v.CurrentURL = "/"
	}
	if err != nil || !v.Enabled {
		return v, err
	}
	if !before.IsZero() {
		events = slices.DeleteFunc(events, func(e AuditEvent) bool { return e.At.After(before) })
	}
	slices.SortFunc(events, func(a, b AuditEvent) int {
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ParentID, b.ParentID), cmp.Compare(a.GrantID, b.GrantID), cmp.Compare(a.Binding, b.Binding), cmp.Compare(a.Reason, b.Reason))
	})
	v.Limited = len(events) > limit
	events = events[:min(len(events), limit)]
	p := s.data.Projects[project]
	labels := map[string]string{}
	names := map[string]string{}
	bindings := map[string]AutoBinding{}
	if !history {
		for _, token := range p.Tokens {
			if token.Kind == tokenProject {
				labels[token.ID] = token.Label
			} else {
				names[token.ID] = token.Nickname
			}
		}
		for _, binding := range p.AutoBindings {
			bindings[binding.GrantID] = binding
		}
	}
	unknown, previous := false, false
	kinds := map[string]string{"request": "audit.event.request", "grant": "audit.event.grant", "registration": "audit.event.registration", "rejected": "audit.event.rejected", "revoked": "audit.event.revoked", "expired": "audit.event.expired", "detached": "audit.event.detached", "suspended": "audit.event.suspended", "unsuspended": "audit.event.unsuspended"}
	event := func(kind string) string {
		if key, ok := kinds[kind]; ok {
			return i18n.T(language, key)
		}
		return kind
	}
	codes := func(scopes []string) string {
		if len(scopes) == 0 {
			return i18n.T(language, "audit.none")
		}
		return strings.Join(scopes, ", ")
	}
	notRecorded, noInformation := i18n.T(language, "audit.not_recorded"), i18n.T(language, "audit.no_information")
	for _, e := range events {
		label := ""
		if history {
			label = historyLabel(language, e.ProjectGenerationID)
			unknown = unknown || e.ProjectGenerationID == ""
			previous = previous || e.ProjectGenerationID != ""
		}

		if a := e.TokenScopes; a != nil {
			name := a.Label
			if name == "" {
				name = i18n.T(language, "audit.project_token")
			}
			reason := i18n.T(language, "audit.scope_reason", "actor", a.ActorID, "added", codes(scopeDifference(a.After, a.Before)), "removed", codes(scopeDifference(a.Before, a.After)), "count", a.ChangedChildren)
			v.Rows = append(v.Rows, directAuditRow{Project: e.Project, GenerationLabel: label, At: activityTime(e.At), AtLabel: e.At.UTC().Format("2006-01-02 15:04:05 UTC"), Kind: e.Kind, Event: i18n.T(language, "audit.event.scopes"), Name: name, Parent: name, Registration: i18n.T(language, "audit.not_applicable"), Peer: notRecorded, Reason: reason})
			continue
		}
		if h := e.Handover; h != nil {
			v.Rows = append(v.Rows, directAuditRow{Project: e.Project, GenerationLabel: label, At: activityTime(e.At), AtLabel: e.At.UTC().Format("2006-01-02 15:04:05 UTC"), Kind: e.Kind, Event: event(e.Kind), Name: h.Name, Registration: h.RegistrationID, Peer: notRecorded})
			continue
		}
		if m := e.Management; m != nil {
			parent := m.TargetParentLabel
			if parent == "" {
				parent = noInformation
			}
			actor := m.ActorName
			if actor == "panel" {
				actor = i18n.T(language, "audit.panel")
			}
			v.Rows = append(v.Rows, directAuditRow{Project: e.Project, GenerationLabel: label, At: activityTime(e.At), AtLabel: e.At.UTC().Format("2006-01-02 15:04:05 UTC"), Kind: e.Kind, Event: event(e.Kind), Name: m.TargetName, Registration: m.TargetRegistrationID, Parent: parent, Peer: notRecorded, Reason: i18n.T(language, "audit.by", "actor", actor)})
			continue
		}
		row := directAuditRow{Project: e.Project, GenerationLabel: label, At: activityTime(e.At), AtLabel: e.At.UTC().Format("2006-01-02 15:04:05 UTC"), Kind: e.Kind, Event: event(e.Kind), Name: names[e.GrantID], Parent: labels[e.ParentID], Reason: e.Reason, Peer: notRecorded}
		if binding, ok := bindings[e.GrantID]; ok {
			row.Registration = binding.RegistrationID
			if row.Registration != "" && !binding.DetachPending && s.canManageSessionLocked(actor, project, binding.Identity, binding.OwnerID) {
				row.SessionURL = (&url.URL{Path: "/projects/sessions", RawQuery: url.Values{"project": {project}}.Encode(), Fragment: "session-registration:" + row.Registration}).String()
			}
		}
		if row.Name == "" {
			row.Name = noInformation
		}
		if row.Registration == "" {
			row.Registration = noInformation
		}
		if row.Parent == "" {
			row.Parent = noInformation
		}
		v.Rows = append(v.Rows, row)
	}
	v.Notice = historyNotice(language, unknown, previous)
	return v, nil
}

func (p *panel) directAudit(w http.ResponseWriter, r *http.Request, session loginSession) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	projects := query["project"]
	bucket, validBucket := historyBucket(query)
	allowed := true
	for key := range query {
		if key != "project" && key != "bucket" {
			allowed = false
		}
	}
	if err != nil || !allowed || !validBucket || (len(projects) == 0 && bucket == "current") || len(projects) > 1 || (len(projects) == 1 && !projectName.MatchString(projects[0])) {
		http.Error(w, "invalid project", 400)
		return
	}
	project := query.Get("project")
	user, ok := p.store.User(session.userID)
	if !ok || !user.Enabled || ((bucket != "current" || project == "") && user.Role != "admin") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	language := p.requestLanguage(r, user.ID)
	audit, err := p.store.directAuditViewBucketFor(user.ID, project, bucket, language)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	v := page{Lang: language, View: "audit", User: user, CSRF: session.csrf, Project: project, Role: p.store.ProjectRole(user.ID, project), Projects: []Project{{Name: project}}, DirectAudit: audit, Roles: map[string]string{}}
	for _, item := range v.Projects {
		v.Roles[item.Name] = p.store.ProjectRole(user.ID, item.Name)
	}
	if !audit.History && project != "" {
		v.Error = p.projectDataNotice(r, project)
	}
	p.render(w, r, 200, v)
}
