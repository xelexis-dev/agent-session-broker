package bugreport

import "errors"

var ErrForbidden = errors.New("bug report access denied")

// ProjectPolicy acquires the control read lock BEFORE the report lock, returning
// a stable current-generation snapshot and its release. Empty actor is only for
// internal current-only operations. A non-empty actor must be an active admin.
type ProjectPolicy func(actor string) (map[string]string, func(), error)

// SetProjectPolicy is configured before serving. It may not call this store.
func (s *Store) SetProjectPolicy(policy ProjectPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectPolicy = policy
}
func (s *Store) policyLease(actor string, requireAdmin bool) (map[string]string, func(), error) {
	s.mu.Lock()
	policy := s.projectPolicy
	s.mu.Unlock()
	if requireAdmin && (actor == "" || policy == nil) {
		return nil, nil, ErrForbidden
	}
	if policy == nil {
		return nil, func() {}, nil
	}
	return policy(actor)
}
func recordBucket(r Report, current map[string]string) string {
	if r.Reporter.ProjectGenerationID == "" {
		return "unknown"
	}
	if current == nil {
		return "current"
	}
	// Stored display names are redacted. Lifetime identity is the opaque ID,
	// never a redacted name that could collide with another project's display.
	for _, id := range current {
		if id == r.Reporter.ProjectGenerationID {
			return "current"
		}
	}
	return "previous"
}
func validBucket(bucket string) bool {
	return bucket == "" || bucket == "current" || bucket == "previous" || bucket == "unknown" || bucket == "history"
}
func matchesBucket(want, got string) bool {
	if want == "" {
		want = "current"
	}
	return want == got || (want == "history" && got != "current")
}
func (s *Store) ListFor(actor string, filter Filter, page int) ([]Report, int, error) {
	return s.list(actor, filter, page, true)
}
func (s *Store) GetFor(actor, id, bucket string) (Report, error) {
	if !validBucket(bucket) {
		return Report{}, ErrInvalid
	}
	current, release, err := s.policyLease(actor, true)
	if err != nil {
		return Report{}, err
	}
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLockedBucket(id, bucket, current)
}
