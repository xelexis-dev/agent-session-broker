package bugreport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var ErrConflict = errors.New("bug report version conflict")

// Actor is server-owned authentication evidence, never caller input.
type Actor struct {
	UserID                       string
	Source                       string
	Session                      *ChangeSession
	Project, ProjectGenerationID string
}
type ChangeSession struct {
	Name           string `json:"name"`
	RegistrationID string `json:"registrationId"`
}
type StatusChange struct {
	At      time.Time      `json:"at"`
	From    string         `json:"from"`
	To      string         `json:"to"`
	Actor   string         `json:"actor"`
	Source  string         `json:"source"`
	Session *ChangeSession `json:"session,omitempty"`
	Note    string         `json:"note,omitempty"`
}

func recordVersion(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (r Report) State() string {
	if r.Status == "closed" && r.Resolution != "" {
		return r.Resolution
	}
	return r.Status
}
func validResolution(r Report) bool {
	return validStatus(r.Status) && (r.Resolution == "" || r.Status == "closed" && (r.Resolution == "resolved" || r.Resolution == "ignored"))
}
func validNote(note string) bool {
	if !utf8.ValidString(note) || utf8.RuneCountInString(note) > 500 {
		return false
	}
	for _, r := range note {
		if unicode.IsControl(r) && r != '\n' {
			return false
		}
	}
	return true
}
func validState(state string) bool {
	return state == "open" || state == "resolved" || state == "ignored" || state == "closed" || state == "all"
}
func (s *Store) SetResolution(id, state, note string, actor Actor, expectedVersion string) (Report, bool, error) {
	if state != "open" && state != "resolved" && state != "ignored" {
		return Report{}, false, ErrInvalid
	}
	return s.mutate(id, state, &note, actor, expectedVersion, true, true)
}

// SetStatusVersion keeps the old panel toggle while sharing history and CAS.
func (s *Store) SetStatusVersion(id, status, actor, expectedVersion string) (Report, bool, error) {
	if !validStatus(status) {
		return Report{}, false, ErrInvalid
	}
	return s.mutate(id, status, nil, Actor{UserID: actor, Source: "panel"}, expectedVersion, true, true)
}
func (s *Store) mutate(id, state string, note *string, actor Actor, expectedVersion string, requireVersion, admin bool) (Report, bool, error) {
	if s.unavailable {
		return Report{}, false, ErrUnavailable
	}
	if strings.TrimSpace(actor.UserID) == "" || (actor.Source != "panel" && actor.Source != "mcp") || requireVersion && expectedVersion == "" || note != nil && !validNote(*note) {
		return Report{}, false, ErrInvalid
	}
	if len(actor.UserID) > 128 || actor.Session != nil && (len(actor.Session.Name) > 128 || len(actor.Session.RegistrationID) > 128) {
		return Report{}, false, ErrInvalid
	}
	if actor.Source == "mcp" && (actor.Session == nil || actor.Session.Name == "" || actor.Session.RegistrationID == "" || actor.Project == "" || actor.ProjectGenerationID == "") {
		return Report{}, false, ErrInvalid
	}
	current, release, err := s.policyLease(actor.UserID, admin)
	if err != nil {
		return Report{}, false, err
	}
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if actor.Source == "mcp" && (current == nil || current[actor.Project] != actor.ProjectGenerationID) {
		return Report{}, false, ErrForbidden
	}
	r, err := s.getLockedBucket(id, "current", current)
	if err != nil {
		return Report{}, false, err
	}
	if actor.Source == "mcp" && r.Reporter.ProjectGenerationID != actor.ProjectGenerationID {
		return Report{}, false, ErrNotFound
	}
	if requireVersion && r.ExpectedVersion != expectedVersion {
		return Report{}, false, ErrConflict
	}
	nextNote := r.Note
	if note != nil {
		nextNote = Redact(*note)
	}
	// A legacy close must preserve the more precise classification already stored.
	if state == "closed" && r.Status == "closed" {
		state = r.State()
	}
	if state == r.State() && nextNote == r.Note {
		return r, false, nil
	}
	event := StatusChange{At: s.now().UTC(), From: r.State(), To: state, Actor: Redact(actor.UserID), Source: actor.Source, Note: nextNote}
	if actor.Session != nil {
		event.Session = &ChangeSession{Name: Redact(actor.Session.Name), RegistrationID: Redact(actor.Session.RegistrationID)}
	}
	r.Status, r.Resolution = state, ""
	if state == "resolved" || state == "ignored" {
		r.Status, r.Resolution = "closed", state
	}
	r.Note = nextNote
	r.StatusChangedAt, r.StatusChangedBy = event.At, event.Actor
	r.StatusChanges = append(r.StatusChanges, event)
	for {
		history, err := json.Marshal(r.StatusChanges)
		if err != nil {
			return Report{}, false, ErrInvalid
		}
		if len(r.StatusChanges) <= 20 && len(history) <= 16*1024 {
			break
		}
		if len(r.StatusChanges) == 1 {
			return Report{}, false, ErrInvalid
		}
		r.StatusChanges = r.StatusChanges[1:]
		r.HistoryTruncated = true
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > MaxRecordBytes {
		return Report{}, false, ErrInvalid
	}
	if s.db.Put(id, raw) != nil {
		return Report{}, false, ErrUnavailable
	}
	r.ExpectedVersion = recordVersion(raw)
	return r, true, nil
}

// Project reads hold the same admin/current-generation policy lease as writes.
func (s *Store) GetProject(actor, project, generation, id string) (Report, error) {
	current, release, err := s.policyLease(actor, true)
	if err != nil {
		return Report{}, err
	}
	defer release()
	if generation == "" || current[project] != generation {
		return Report{}, ErrForbidden
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.getLockedBucket(id, "current", current)
	if err != nil {
		return Report{}, err
	}
	if r.Reporter.ProjectGenerationID != generation {
		return Report{}, ErrNotFound
	}
	return r, nil
}
func (s *Store) ListProject(actor, project, generation, state, severity string, page int) ([]Report, int, error) {
	if state == "" {
		state = "open"
	}
	if !validState(state) || severity != "" && !validSeverity(severity) || page < 1 {
		return nil, 0, ErrInvalid
	}
	current, release, err := s.policyLease(actor, true)
	if err != nil {
		return nil, 0, err
	}
	defer release()
	if generation == "" || current[project] != generation {
		return nil, 0, ErrForbidden
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.rowsLocked()
	if err != nil {
		return nil, 0, err
	}
	out := []Report{}
	for _, r := range rows {
		if !validResolution(r) {
			return nil, 0, ErrUnavailable
		}
		if r.Reporter.ProjectGenerationID != generation || s.now().Sub(r.ReceivedAt) >= Retention || state != "all" && state != r.State() || severity != "" && severity != r.Severity {
			continue
		}
		out = append(out, r)
	}
	total := len(out)
	if page > (total+PageSize-1)/PageSize {
		return []Report{}, total, nil
	}
	start := (page - 1) * PageSize
	return out[start:min(start+PageSize, total)], total, nil
}
