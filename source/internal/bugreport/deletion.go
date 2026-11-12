package bugreport

import (
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

var ErrDeleteIncomplete = errors.New("report deleted, but its deletion record was not saved as complete")

type reportDatabase interface {
	Get(string) ([]byte, error)
	Put(string, []byte) error
	List() (map[string][]byte, error)
	Delete(string) error
	Batch(map[string][]byte, []string) error
	Close() error
}

type Deletion struct {
	Version             int       `json:"v"`
	ReportID            string    `json:"reportId"`
	Phase               string    `json:"phase"`
	DeletedAt           time.Time `json:"deletedAt"`
	Actor               string    `json:"actor"`
	Source              string    `json:"source"`
	ProjectGenerationID string    `json:"projectGenerationId"`
	ReceivedAt          time.Time `json:"receivedAt"`
	Severity            string    `json:"severity"`
	State               string    `json:"state"`
}

func (s *Store) deletionDBLocked() (reportDatabase, error) {
	if s.unavailable || s.closed {
		return nil, ErrUnavailable
	}
	if s.deletions == nil {
		db, err := sqlitestore.Open(s.directory, "deletions.db")
		if err != nil {
			return nil, ErrUnavailable
		}
		s.deletions = db
	}
	return s.deletions, nil
}
func decodeDeletion(id string, raw []byte) (Deletion, error) {
	var d Deletion
	if json.Unmarshal(raw, &d) != nil || d.Version != 1 || d.ReportID != id || !strings.HasPrefix(id, "bug-") || (d.Phase != "requested" && d.Phase != "deleted") || d.DeletedAt.IsZero() || d.Actor == "" || d.Source != "panel" || !validSeverity(d.Severity) || !validState(d.State) || d.State == "all" {
		return Deletion{}, ErrUnavailable
	}
	return d, nil
}
func (s *Store) deletionRowsLocked() (map[string]Deletion, error) {
	db, err := s.deletionDBLocked()
	if err != nil {
		return nil, err
	}
	rows, err := db.List()
	if err != nil {
		return nil, ErrUnavailable
	}
	out := make(map[string]Deletion, len(rows))
	for id, raw := range rows {
		d, err := decodeDeletion(id, raw)
		if err != nil {
			return nil, err
		}
		out[id] = d
	}
	return out, nil
}
func (s *Store) writeDeletionLocked(d Deletion) error {
	rows, err := s.deletionRowsLocked()
	if err != nil {
		return err
	}
	delete(rows, d.ReportID)
	kept := make([]Deletion, 0, len(rows))
	var deletes []string
	for _, r := range rows {
		if s.now().Sub(r.DeletedAt) >= Retention {
			deletes = append(deletes, r.ReportID)
		} else {
			kept = append(kept, r)
		}
	}
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].DeletedAt.Equal(kept[j].DeletedAt) {
			return kept[i].ReportID > kept[j].ReportID
		}
		return kept[i].DeletedAt.After(kept[j].DeletedAt)
	})
	for _, r := range kept[min(len(kept), MaxReports-1):] {
		deletes = append(deletes, r.ReportID)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return ErrUnavailable
	}
	if s.deletions.Batch(map[string][]byte{d.ReportID: raw}, deletes) != nil {
		return ErrUnavailable
	}
	return nil
}

// Delete holds the admin policy lease and report mutation lock through both journals.
// A requested record never proves that an administrator actually deleted a report.
func (s *Store) Delete(actor, id, expectedVersion string) (Deletion, error) {
	_, release, err := s.policyLease(actor, true)
	if err != nil {
		return Deletion{}, err
	}
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return Deletion{}, ErrUnavailable
	}
	if expectedVersion == "" {
		return Deletion{}, ErrInvalid
	}
	raw, err := s.db.Get(id)
	if errors.Is(err, fs.ErrNotExist) || err == nil && len(raw) == 0 {
		return Deletion{}, ErrNotFound
	}
	if err != nil {
		return Deletion{}, ErrUnavailable
	}
	var r Report
	if json.Unmarshal(raw, &r) != nil || r.ID != id || !strings.HasPrefix(id, "bug-") || !validResolution(r) {
		return Deletion{}, ErrUnavailable
	}
	if s.now().Sub(r.ReceivedAt) >= Retention {
		return Deletion{}, ErrNotFound
	}
	if recordVersion(raw) != expectedVersion {
		return Deletion{}, ErrConflict
	}
	d := Deletion{Version: 1, ReportID: id, Phase: "requested", DeletedAt: s.now().UTC(), Actor: Redact(actor), Source: "panel", ProjectGenerationID: r.Reporter.ProjectGenerationID, ReceivedAt: r.ReceivedAt, Severity: r.Severity, State: r.State()}
	if err = s.writeDeletionLocked(d); err != nil {
		return Deletion{}, err
	}
	if s.db.Delete(id) != nil {
		return Deletion{}, ErrUnavailable
	}
	d.Phase = "deleted"
	if err = s.writeDeletionLocked(d); err != nil {
		d.Phase = "requested"
		return d, ErrDeleteIncomplete
	}
	return d, nil
}
func (s *Store) DeletionFor(actor, id string) (Deletion, error) {
	_, release, err := s.policyLease(actor, true)
	if err != nil {
		return Deletion{}, err
	}
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.deletionDBLocked()
	if err != nil {
		return Deletion{}, err
	}
	raw, err := db.Get(id)
	if errors.Is(err, fs.ErrNotExist) || err == nil && len(raw) == 0 {
		return Deletion{}, ErrNotFound
	}
	if err != nil {
		return Deletion{}, ErrUnavailable
	}
	d, err := decodeDeletion(id, raw)
	if err != nil {
		return Deletion{}, err
	}
	if s.now().Sub(d.DeletedAt) >= Retention {
		return Deletion{}, ErrNotFound
	}
	return d, nil
}
func (s *Store) deleteProjectDeletionsLocked(generation string) error {
	db, err := s.deletionDBLocked()
	if err != nil {
		serverdiag.Emit(serverdiag.Event{Type: "project-deletion-warning", Reason: "unavailable-journal"})
		return nil
	}
	rows, err := db.List()
	if err != nil {
		serverdiag.Emit(serverdiag.Event{Type: "project-deletion-warning", Reason: "unreadable-journal"})
		return nil
	}
	var deletes []string
	for id, raw := range rows {
		var record struct {
			ProjectGenerationID string `json:"projectGenerationId"`
		}
		if json.Unmarshal(raw, &record) != nil || record.ProjectGenerationID == "" {
			serverdiag.Emit(serverdiag.Event{Type: "project-deletion-warning", Reason: "undecodable-row"})
			continue
		}
		if record.ProjectGenerationID == generation {
			deletes = append(deletes, id)
		}
	}
	if len(deletes) != 0 && db.Batch(nil, deletes) != nil {
		serverdiag.Emit(serverdiag.Event{Type: "project-deletion-warning", Reason: "journal-cleanup"})
	}
	return nil
}
