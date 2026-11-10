// Package bugreport owns the instance-wide, private defect report store.
package bugreport

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport/clientdiag"
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

const PageSize = 50
const MaxReports = 2000
const MaxBytes = 32 * 1024
const MaxRecordBytes = 64 * 1024
const Retention = 180 * 24 * time.Hour

var ErrNotFound = errors.New("bug report not found")
var ErrUnavailable = errors.New("bug report storage is unavailable")
var ErrInvalid = errors.New("invalid bug report")
var ErrRateLimited = errors.New("rate-limited")

type RateLimitError struct{ RetryAt time.Time }

func (e *RateLimitError) Error() string {
	return "rate-limited; retry at " + e.RetryAt.UTC().Format(time.RFC3339)
}
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

type DiagnosticEvent = clientdiag.DiagnosticEvent
type ClientDiagnostics = clientdiag.ClientDiagnostics
type Input struct {
	Title    string             `json:"title"`
	Summary  string             `json:"summary"`
	Steps    string             `json:"steps,omitempty"`
	Expected string             `json:"expected,omitempty"`
	Actual   string             `json:"actual,omitempty"`
	Severity string             `json:"severity,omitempty"`
	Tool     string             `json:"tool,omitempty"`
	Client   *ClientDiagnostics `json:"client,omitempty"`
}
type Reporter struct {
	ProjectGenerationID string `json:"projectGenerationId,omitempty"`
	Project             string `json:"project"`
	SessionName         string `json:"sessionName"`
	RegistrationID      string `json:"registrationId"`
	Harness             string `json:"harness"`
	OwnerID             string `json:"ownerId"`
}
type Report struct {
	Input
	ID               string         `json:"id"`
	Reporter         Reporter       `json:"reporter"`
	ReceivedAt       time.Time      `json:"receivedAt"`
	ServerVersion    string         `json:"serverVersion"`
	Status           string         `json:"status"`
	Resolution       string         `json:"resolution,omitempty"`
	Note             string         `json:"note,omitempty"`
	StatusChanges    []StatusChange `json:"statusChanges,omitempty"`
	HistoryTruncated bool           `json:"historyTruncated,omitempty"`
	ExpectedVersion  string         `json:"-"`
	StatusChangedAt  time.Time      `json:"statusChangedAt,omitempty"`
	StatusChangedBy  string         `json:"statusChangedBy,omitempty"`
}
type Filter struct{ Status, State, Severity, Bucket string }
type Store struct {
	mu            sync.Mutex
	db            reportDatabase
	deletions     reportDatabase
	closed        bool
	directory     string
	now           func() time.Time
	unavailable   bool
	projectExists func(string) bool
	projectPolicy ProjectPolicy
}

// Open receives the instance memory directory, never a project directory.
func Open(memoryDir string) (*Store, error) {
	db, err := sqlitestore.Open(filepath.Join(memoryDir, "bugreports"), "bugreports.db")
	if err != nil {
		if errors.Is(err, sqlitestore.ErrCorrupt) {
			return nil, errors.Join(ErrUnavailable, sqlitestore.ErrCorrupt)
		}
		return nil, ErrUnavailable
	}
	return &Store{db: db, directory: filepath.Join(memoryDir, "bugreports"), now: time.Now}, nil
}

// Unavailable keeps the fixed API error without opening or mutating the corrupt
// database. A fresh Open on a later process start is the only recovery attempt.
func Unavailable() *Store            { return &Store{unavailable: true, now: time.Now} }
func (s *Store) IsUnavailable() bool { return s != nil && s.unavailable }
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	var deletionErr error
	if s.deletions != nil {
		deletionErr = s.deletions.Close()
	}
	return errors.Join(s.db.Close(), deletionErr)
}

// SetProjectFilter is an additional display-name filter for standalone stores.
// Managed runtimes use SetProjectPolicy instead. This callback must not acquire
// control-store locks or call back into the report store.
func (s *Store) SetProjectFilter(exists func(string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectExists = exists
}
func (s *Store) visibleProject(project string) bool {
	return s.projectExists == nil || s.projectExists(project)
}

var tokenPattern = regexp.MustCompile(`asb_[psu]_[A-Za-z0-9_-]+`)

func Redact(s string) string { return tokenPattern.ReplaceAllString(s, "<redacted>") }
func validSeverity(s string) bool {
	return s == "low" || s == "medium" || s == "high" || s == "critical"
}
func validStatus(s string) bool { return s == "open" || s == "closed" }
func Validate(in Input) error {
	for _, f := range []struct {
		s        string
		max      int
		required bool
	}{{in.Title, 200, true}, {in.Summary, 4000, true}, {in.Steps, 4000, false}, {in.Expected, 2000, false}, {in.Actual, 2000, false}, {in.Tool, 64, false}} {
		if !utf8.ValidString(f.s) || utf8.RuneCountInString(f.s) > f.max || (f.required && strings.TrimSpace(f.s) == "") {
			return ErrInvalid
		}
	}
	if in.Severity != "" && !validSeverity(in.Severity) {
		return ErrInvalid
	}
	raw, err := json.Marshal(in)
	if err != nil || len(raw) > MaxBytes {
		return ErrInvalid
	}
	if in.Client != nil && len(in.Client.Events) > 20 {
		return ErrInvalid
	}
	return nil
}
func (s *Store) rowsLocked() ([]Report, error) {
	if s.unavailable {
		return nil, ErrUnavailable
	}
	rows, err := s.db.List()
	if err != nil {
		return nil, errors.New("bug report store unavailable")
	}
	out := make([]Report, 0, len(rows))
	for key, raw := range rows {
		if strings.HasPrefix(key, "quota-") {
			if !validQuota(raw) {
				return nil, errors.New("bug report quota invalid")
			}
			continue
		}
		var r Report
		if json.Unmarshal(raw, &r) != nil || r.ID != key || !strings.HasPrefix(key, "bug-") || !validResolution(r) {
			return nil, errors.New("bug report store invalid")
		}
		r.ExpectedVersion = recordVersion(raw)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ReceivedAt.Equal(out[j].ReceivedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].ReceivedAt.After(out[j].ReceivedAt)
	})
	return out, nil
}
func (s *Store) Create(in Input, reporter Reporter, version string) (Report, error) {
	if s.unavailable {
		return Report{}, ErrUnavailable
	}
	if err := Validate(in); err != nil {
		return Report{}, err
	}
	if reporter.ProjectGenerationID == "" || reporter.Project == "" || reporter.RegistrationID == "" || reporter.SessionName == "" || reporter.Harness == "" || reporter.OwnerID == "" {
		return Report{}, ErrInvalid
	}
	if in.Severity == "" {
		in.Severity = "medium"
	}
	current, release, err := s.policyLease("", false)
	if err != nil {
		return Report{}, err
	}
	defer release()
	if current != nil && current[reporter.Project] != reporter.ProjectGenerationID {
		return Report{}, ErrForbidden
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	rows, err := s.rowsLocked()
	if err != nil {
		return Report{}, err
	}
	quota, err := s.quotaLocked(reporter.Project, reporter.ProjectGenerationID, rows, now)
	if err != nil {
		return Report{}, err
	}
	var hourly, daily []time.Time
	for _, event := range quota.Events {
		daily = append(daily, event.At)
		if event.RegistrationHash == quotaHash(reporter.RegistrationID) && now.Sub(event.At) < time.Hour {
			hourly = append(hourly, event.At)
		}
	}
	sort.Slice(hourly, func(i, j int) bool { return hourly[i].Before(hourly[j]) })
	sort.Slice(daily, func(i, j int) bool { return daily[i].Before(daily[j]) })
	var retry time.Time
	if len(hourly) >= 10 {
		retry = hourly[len(hourly)-10].Add(time.Hour)
	}
	if len(daily) >= 200 {
		at := daily[len(daily)-200].Add(24 * time.Hour)
		if at.After(retry) {
			retry = at
		}
	}
	if !retry.IsZero() {
		return Report{}, &RateLimitError{retry}
	}
	report := Report{Input: in, ID: "bug-" + rand.Text(), Reporter: reporter, ReceivedAt: now, ServerVersion: version, Status: "open"}
	raw, err := json.Marshal(report)
	if err != nil || len(raw) > MaxRecordBytes {
		return Report{}, ErrInvalid
	}
	// Redact the complete server-owned record, including every nested field.
	raw = []byte(Redact(string(raw)))
	if json.Unmarshal(raw, &report) != nil {
		return Report{}, ErrInvalid
	}
	var deletes []string
	kept := 1
	for _, r := range rows {
		if now.Sub(r.ReceivedAt) >= Retention || kept >= MaxReports {
			deletes = append(deletes, r.ID)
		} else {
			kept++
		}
	}
	quota.Events = append(quota.Events, quotaEvent{quotaHash(reporter.RegistrationID), now})
	quotaRaw, err := json.Marshal(quota)
	if err != nil {
		return Report{}, ErrInvalid
	}
	if s.db.Batch(map[string][]byte{report.ID: raw, quotaKey(reportScope(reporter.Project, reporter.ProjectGenerationID)): quotaRaw}, deletes) != nil {
		return Report{}, errors.New("bug report write failed")
	}
	report.ExpectedVersion = recordVersion(raw)
	return report, nil
}
func (s *Store) List(filter Filter, page int) ([]Report, int, error) {
	if filter.Bucket != "" && filter.Bucket != "current" {
		return nil, 0, ErrForbidden
	}
	return s.list("", filter, page, false)
}
func (s *Store) list(actor string, filter Filter, page int, admin bool) ([]Report, int, error) {
	if !validBucket(filter.Bucket) {
		return nil, 0, ErrInvalid
	}
	if (filter.State != "" && !validState(filter.State)) || (filter.Status != "" && !validStatus(filter.Status)) || (filter.Severity != "" && !validSeverity(filter.Severity)) {
		return nil, 0, ErrInvalid
	}
	if page < 1 {
		page = 1
	}
	current, release, err := s.policyLease(actor, admin)
	if err != nil {
		return nil, 0, err
	}
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.rowsLocked()
	if err != nil {
		return nil, 0, err
	}
	out := []Report{}
	now := s.now()
	for _, r := range rows {
		if !matchesBucket(filter.Bucket, recordBucket(r, current)) || ((filter.Bucket == "" || filter.Bucket == "current") && !s.visibleProject(r.Reporter.Project)) || now.Sub(r.ReceivedAt) >= Retention || (filter.Status != "" && r.Status != filter.Status) || (filter.State != "" && filter.State != "all" && r.State() != filter.State) || (filter.Severity != "" && r.Severity != filter.Severity) {
			continue
		}
		out = append(out, r)
	}
	total := len(out)
	if page > (total+PageSize-1)/PageSize {
		return []Report{}, total, nil
	}
	start := (page - 1) * PageSize
	end := min(start+PageSize, total)
	return out[start:end], total, nil
}
func (s *Store) Get(id string) (Report, error) {
	current, release, err := s.policyLease("", false)
	if err != nil {
		return Report{}, err
	}
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLockedBucket(id, "current", current)
}
func (s *Store) getLockedBucket(id, bucket string, current map[string]string) (Report, error) {
	if s.unavailable {
		return Report{}, ErrUnavailable
	}
	if !strings.HasPrefix(id, "bug-") {
		return Report{}, ErrNotFound
	}
	raw, err := s.db.Get(id)
	if errors.Is(err, fs.ErrNotExist) {
		return Report{}, ErrNotFound
	}
	if err != nil {
		return Report{}, errors.New("bug report store unavailable")
	}
	var r Report
	if len(raw) == 0 {
		return Report{}, ErrNotFound
	}
	if json.Unmarshal(raw, &r) != nil || r.ID != id {
		return Report{}, errors.New("bug report store invalid")
	}
	if !matchesBucket(bucket, recordBucket(r, current)) || ((bucket == "" || bucket == "current") && !s.visibleProject(r.Reporter.Project)) || s.now().Sub(r.ReceivedAt) >= Retention {
		return Report{}, ErrNotFound
	}
	r.ExpectedVersion = recordVersion(raw)
	if !validResolution(r) {
		return Report{}, ErrUnavailable
	}
	return r, nil
}

// SetStatus is retained for internal legacy callers. HTTP and MCP require a version.
func (s *Store) SetStatus(id, status, actorUserID string) error {
	if !validStatus(status) {
		return ErrInvalid
	}
	_, _, err := s.mutate(id, status, nil, Actor{UserID: actorUserID, Source: "panel"}, "", false, false)
	return err
}
func (s *Store) DeleteProject(project, generation string) error {
	if generation == "" {
		return ErrInvalid
	}
	_, release, err := s.policyLease("", false)
	if err != nil {
		return err
	}
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.rowsLocked()
	if err != nil {
		return err
	}
	deletes := []string{quotaKey(reportScope(project, generation))}
	for _, r := range rows {
		if r.Reporter.Project == Redact(project) && r.Reporter.ProjectGenerationID == generation {
			deletes = append(deletes, r.ID)
		}
	}
	if s.db.Batch(nil, deletes) != nil {
		return errors.New("bug report delete failed")
	}
	return s.deleteProjectDeletionsLocked(generation)
}
