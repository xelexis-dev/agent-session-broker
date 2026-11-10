package controlpanel

import (
	"crypto/rand"
	"maps"

	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
)

const projectGenerationVersion = 5

func validProjectGenerations(st state) bool {
	seen := map[string]bool{}
	for _, p := range st.Projects {
		id := p.ProjectGenerationID
		if st.Version < projectGenerationVersion {
			if id != "" {
				return false
			}
			continue
		}
		if !registrationNickname.MatchString(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}
func (s *Store) migrateProjectGenerations() error {
	if s.data.Version >= projectGenerationVersion {
		return nil
	}
	candidate := s.data
	candidate.Version = projectGenerationVersion
	candidate.Projects = maps.Clone(s.data.Projects)
	for name, p := range candidate.Projects {
		p.ProjectGenerationID = rand.Text()
		candidate.Projects[name] = p
	}
	return s.commit(candidate)
}

// ProjectGeneration returns server-owned identity, including during deletion.
func (s *Store) ProjectGeneration(project string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ""
	}
	return s.data.Projects[project].ProjectGenerationID
}

// BugReportPolicy acquires control before report locks. The caller MUST hold
// release through the complete report operation, including its SQLite commit.
// Empty actor is reserved for server-owned current-only operations; historical
// readers must supply an active instance administrator.
func (s *Store) BugReportPolicy(actor string) (map[string]string, func(), error) {
	s.mu.RLock()
	if s.closed || (actor != "" && !s.isAdminLocked(actor)) {
		s.mu.RUnlock()
		return nil, nil, bugreport.ErrForbidden
	}
	generations := map[string]string{}
	for name, p := range s.data.Projects {
		if !p.Deleting {
			generations[name] = p.ProjectGenerationID
		}
	}
	return generations, s.mu.RUnlock, nil
}

type AuditFilter struct{ Bucket string }

func auditBucket(e AuditEvent, projects map[string]Project) string {
	if e.ProjectGenerationID == "" {
		return "unknown"
	}
	if p, ok := projects[e.Project]; ok && p.ProjectGenerationID == e.ProjectGenerationID {
		return "current"
	}
	return "previous"
}
func validRecordBucket(bucket string) bool {
	return bucket == "" || bucket == "current" || bucket == "previous" || bucket == "unknown" || bucket == "history"
}
func matchesRecordBucket(want, got string) bool {
	if want == "" {
		want = "current"
	}
	return want == got || (want == "history" && got != "current")
}

// DirectAuditRecordsFor exposes history only to instance administrators.
// project="" selects all projects for an administrator; ordinary readers must
// identify a current project and retain the Owner/Developer restrictions.
func (s *Store) DirectAuditRecordsFor(actor, project string, filter AuditFilter) ([]AuditEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.directAuditRecordsLocked(actor, project, filter)
}
func (s *Store) directAuditRecordsLocked(actor, project string, filter AuditFilter) ([]AuditEvent, error) {
	if s.closed || !validRecordBucket(filter.Bucket) {
		return nil, ErrForbidden
	}
	current := filter.Bucket == "" || filter.Bucket == "current"
	admin := s.isAdminLocked(actor)
	role := s.projectRoleLocked(actor, project)
	if (!current || project == "") && !admin {
		return nil, ErrForbidden
	}
	if current && project != "" && role != "owner" && role != "developer" {
		return nil, ErrForbidden
	}
	if current && !bool(s.data.ScopeIssuanceReady) {
		return nil, nil
	}
	out := []AuditEvent{}
	for _, e := range s.audits {
		if (project == "" || e.Project == project) && (admin || role == "owner" || e.OwnerID == actor) && matchesRecordBucket(filter.Bucket, auditBucket(e, s.data.Projects)) && s.now().Before(e.At.Add(auditRetention)) {
			out = append(out, e)
		}
	}
	sortAuditRecords(out)
	return out, nil
}

// Unknown records survive name-based orphan cleanup; deletion removes exactly
// the departing generation. Known orphan rows still follow the 0.17.1 cleanup.
func pruneGenerationAudit(e AuditEvent, before, after map[string]Project) bool {
	if e.ProjectGenerationID == "" {
		return false
	}
	old, existed := before[e.Project]
	next, retained := after[e.Project]
	if !existed {
		return true
	}
	return (!retained || old.ProjectGenerationID != next.ProjectGenerationID) && e.ProjectGenerationID == old.ProjectGenerationID
}
