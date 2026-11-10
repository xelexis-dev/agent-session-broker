package controlpanel

import (
	"cmp"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"
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
func (s *Store) directAuditViewFor(actor, project string) (directAuditView, error) {
	return s.directAuditViewBucketFor(actor, project, "current")
}

func (s *Store) directAuditViewBucketFor(actor, project, bucket string) (directAuditView, error) {
	return s.directAuditViewWindowFor(actor, project, bucket, time.Time{}, directAuditLimit)
}
func (s *Store) directAuditViewWindowFor(actor, project, bucket string, before time.Time, limit int) (directAuditView, error) {
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
	kinds := map[string]string{"handover": "handover", "request": "요청", "grant": "자격 발급", "registration": "등록", "rejected": "거절", "revoked": "회수", "expired": "만료", "detached": "연결 해제", "suspended": "일시 중지", "unsuspended": "재개"}
	for _, e := range events {
		label := ""
		if history {
			label = historyLabel(e.ProjectGenerationID)
			unknown = unknown || e.ProjectGenerationID == ""
			previous = previous || e.ProjectGenerationID != ""
		}

		if a := e.TokenScopes; a != nil {
			name := a.Label
			if name == "" {
				name = "프로젝트 토큰"
			}
			reason := "실행: " + a.ActorID + " · 추가 " + codesText(scopeDifference(a.After, a.Before)) + " · 제거 " + codesText(scopeDifference(a.Before, a.After)) + " · 변경 자격 " + strconv.Itoa(a.ChangedChildren) + "개"
			v.Rows = append(v.Rows, directAuditRow{Project: e.Project, GenerationLabel: label, At: activityTime(e.At), AtLabel: e.At.UTC().Format("2006-01-02 15:04:05 UTC"), Kind: e.Kind, Event: "권한 변경", Name: name, Parent: name, Registration: "해당 없음", Peer: "저장 안 됨", Reason: reason})
			continue
		}
		if h := e.Handover; h != nil {
			v.Rows = append(v.Rows, directAuditRow{Project: e.Project, GenerationLabel: label, At: activityTime(e.At), AtLabel: e.At.UTC().Format("2006-01-02 15:04:05 UTC"), Kind: e.Kind, Event: "handover", Name: h.Name, Registration: h.RegistrationID, Peer: "저장 안 됨"})
			continue
		}
		if m := e.Management; m != nil {
			parent := m.TargetParentLabel
			if parent == "" {
				parent = "정보 없음"
			}
			actor := m.ActorName
			if actor == "panel" {
				actor = "관리 화면"
			}
			v.Rows = append(v.Rows, directAuditRow{Project: e.Project, GenerationLabel: label, At: activityTime(e.At), AtLabel: e.At.UTC().Format("2006-01-02 15:04:05 UTC"), Kind: e.Kind, Event: kinds[e.Kind], Name: m.TargetName, Registration: m.TargetRegistrationID, Parent: parent, Peer: "저장 안 됨", Reason: "실행: " + actor})
			continue
		}
		row := directAuditRow{Project: e.Project, GenerationLabel: label, At: activityTime(e.At), AtLabel: e.At.UTC().Format("2006-01-02 15:04:05 UTC"), Kind: e.Kind, Event: kinds[e.Kind], Name: names[e.GrantID], Parent: labels[e.ParentID], Reason: e.Reason, Peer: "저장 안 됨"}
		if binding, ok := bindings[e.GrantID]; ok {
			row.Registration = binding.RegistrationID
			if row.Registration != "" && !binding.DetachPending && s.canManageSessionLocked(actor, project, binding.Identity, binding.OwnerID) {
				row.SessionURL = (&url.URL{Path: "/projects/sessions", RawQuery: url.Values{"project": {project}}.Encode(), Fragment: "session-registration:" + row.Registration}).String()
			}
		}
		if row.Name == "" {
			row.Name = "정보 없음"
		}
		if row.Registration == "" {
			row.Registration = "정보 없음"
		}
		if row.Parent == "" {
			row.Parent = "정보 없음"
		}
		v.Rows = append(v.Rows, row)
	}
	v.Notice = historyNotice(unknown, previous)
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
	audit, err := p.store.directAuditViewBucketFor(user.ID, project, bucket)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	v := page{View: "audit", User: user, CSRF: session.csrf, Project: project, Role: p.store.ProjectRole(user.ID, project), Projects: []Project{{Name: project}}, DirectAudit: audit, Roles: map[string]string{}}
	for _, item := range v.Projects {
		v.Roles[item.Name] = p.store.ProjectRole(user.ID, item.Name)
	}
	if !audit.History && project != "" {
		v.Error = p.projectDataNotice(r, project)
	}
	p.render(w, 200, v)
}
