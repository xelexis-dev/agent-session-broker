package clientstate

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

const DiagnosticEventLimit = 256
const DiagnosticByteLimit = 128 * 1024

// DiagnosticEvent accepts identifiers and closed reason vocabularies only.
// It deliberately has no free-form message, error, endpoint or credential field.
type DiagnosticEvent struct {
	extra          map[string]json.RawMessage
	At             time.Time `json:"at"`
	SessionID      string    `json:"sessionId"`
	RegistrationID string    `json:"registrationId,omitempty"`
	Generation     uint64    `json:"generation,omitempty"`
	Event          string    `json:"event"`
	Reason         string    `json:"reason,omitempty"`
	ErrorClass     string    `json:"errorClass,omitempty"`
	AuthState      string    `json:"authState,omitempty"`
}
type DiagnosticSummary struct {
	extra             map[string]json.RawMessage
	UnknownCodes      int       `json:"unknownCodes,omitempty"`
	HandoverSkipped   string    `json:"handoverSkipped,omitempty"`
	Unavailable       bool      `json:"unavailable,omitempty"`
	LastRenewSuccess  time.Time `json:"lastRenewSuccess,omitzero"`
	LastFailureAt     time.Time `json:"lastFailureAt,omitzero"`
	LastFailureReason string    `json:"lastFailureReason,omitempty"`
	LastFailureCode   string    `json:"lastFailureCode,omitempty"`
	LastFailureClass  string    `json:"lastFailureClass,omitempty"`
	LastReceiverExit  string    `json:"lastReceiverExit,omitempty"`
	AuthState         string    `json:"authState,omitempty"`
}
type Diagnostics struct {
	extra   map[string]json.RawMessage
	Format  int               `json:"format,omitempty"`
	Summary DiagnosticSummary `json:"summary"`
	Events  []DiagnosticEvent `json:"events"`
}

var diagnosticKey = regexp.MustCompile(`^[a-f0-9]{32}$`)
var diagnosticSessionID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var diagnosticRegistrationID = regexp.MustCompile(`^[A-Z2-7]{26}$`)
var diagnosticsMu sync.Mutex

const diagnosticSuccessInterval = time.Minute

type diagnosticRenewal struct {
	event     DiagnosticEvent
	writtenAt time.Time
	pending   bool
}

func diagnosticChoice(v string, choices ...string) bool {
	for _, c := range choices {
		if v == c {
			return true
		}
	}
	return false
}
func knownDiagnosticEvent(event string) bool {
	return diagnosticChoice(event, "renew-success", "renew-failed", "receive-failed", "sink-failed", "receiver-exit", "reconnect-start", "reconnect-failed", "reconnected", "auth-transition", "transport-created", "transport-replaced", "probe-failed", "activity-unavailable", "token-changed", "legacy-migration", "handover-skipped", "follow-message-dropped", "codex-stop-missing", "kimi-hook-handoff", "codex-claim-unconfirmed", "codex-claim-confirmed", "codex-claim-mismatch", "device-identity", "steer-recovery-ambiguous", "steer-recovery-acked", "steer-recovery-released", "steer-recovery-pending", "steer-recovery-skipped", "steer-uncertain-retired", "steer-delivered", "steer-rejected", "steer-unverified", "steer-disabled-off", "steer-disabled-unconfirmed-thread", "steer-disabled-ack-required", "steer-disabled-unsupported-codex", "steer-disabled-unverified-host")
}
func knownDeviceIdentityReason(reason string) bool {
	return diagnosticChoice(reason, "ok", "path-unavailable", "directory-insecure", "directory-unwritable", "lock-failed", "secret-insecure", "secret-malformed", "secret-missing", "secret-changed", "secret-create-failed", "process-identity-unavailable", "secret-created", "secret-created-lock-present")
}

// Persisted diagnostics are read strictly; each list is the exact accept set
// of its field, so a new value is added in exactly one place.
var (
	diagnosticErrorClasses    = []string{"", "project-deleted", "project-disabled", "project-deleting", "deadline", "cancelled", "network", "authorization", "protocol", "other", "lease-replaced", "not-activated", "ack-invalid"}
	diagnosticFailureCodes    = append([]string{"invalid", "not-found", "detached", "suspended", "owned", "owned-local", "registration-in-use", "revision-mismatch", "stale-probe", "storage", "invalid-token", "capacity", "identity-mismatch"}, diagnosticErrorClasses...)
	diagnosticReceiverExits   = []string{"", "upstream", "context", "sink", "terminal"}
	diagnosticAuthStates      = []string{"", "unauthorized", "pending", "authorized", "denied", "expired", "rejected", "failed"}
	diagnosticActivityReasons = []string{"unsupported", "evidence-missing", "identity-mismatch", "malformed-evidence", "stale-evidence", "budget-exceeded", "observer-unavailable"}
	diagnosticEventReasons    = slices.Concat([]string{"approval-required", "still-receiving", "secret-mismatch", "client-mismatch", "owner-mismatch", "unsupported-mode", "identity-conflict", "refused"}, diagnosticReceiverExits, diagnosticFailureCodes)
)

func knownDiagnosticReason(reason string) bool {
	return knownDeviceIdentityReason(reason) || diagnosticChoice(reason, diagnosticEventReasons...) || diagnosticChoice(reason, diagnosticActivityReasons...)
}
func validDiagnostic(e DiagnosticEvent) bool {
	activityReason := e.Event == "activity-unavailable" && diagnosticChoice(e.Reason, diagnosticActivityReasons...)
	return !e.At.IsZero() && diagnosticSessionID.MatchString(e.SessionID) && (e.RegistrationID == "" || diagnosticRegistrationID.MatchString(e.RegistrationID)) &&
		knownDiagnosticEvent(e.Event) &&
		(activityReason || (e.Event == "device-identity" && knownDeviceIdentityReason(e.Reason)) || (e.Event != "activity-unavailable" && e.Event != "device-identity" && diagnosticChoice(e.Reason, diagnosticEventReasons...))) &&
		diagnosticChoice(e.ErrorClass, diagnosticErrorClasses...) &&
		diagnosticChoice(e.AuthState, diagnosticAuthStates...)
}
func validSummary(s DiagnosticSummary) bool {
	return diagnosticChoice(s.LastFailureCode, diagnosticFailureCodes...) &&
		(diagnosticChoice(s.LastFailureReason, "renew-failed", "receive-failed", "sink-failed", "reconnect-failed", "probe-failed") || diagnosticChoice(s.LastFailureReason, diagnosticReceiverExits...)) &&
		diagnosticChoice(s.LastFailureClass, diagnosticErrorClasses...) &&
		diagnosticChoice(s.LastReceiverExit, diagnosticReceiverExits...) &&
		diagnosticChoice(s.AuthState, diagnosticAuthStates...)
}
func diagnosticDirectory(path string) bool {
	info, err := os.Lstat(path)
	return errors.Is(err, os.ErrNotExist) || (err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0)
}

func readDiagnostics(path string, v2 bool) (Diagnostics, error) {
	var out Diagnostics
	parent := filepath.Dir(path)
	if !diagnosticDirectory(parent) || (v2 && !diagnosticDirectory(filepath.Dir(parent))) {
		return out, errors.New("diagnostics unavailable")
	}
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil || !before.Mode().IsRegular() {
		return out, errors.New("diagnostics unavailable")
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, errors.New("diagnostics unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return out, errors.New("diagnostics unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(file, DiagnosticByteLimit+1))
	if err != nil || len(raw) > DiagnosticByteLimit || !utf8.Valid(raw) || json.Unmarshal(raw, &out) != nil || len(out.Events) > DiagnosticEventLimit || (v2 && out.Format < 2) || !storedCodes(out.Summary.HandoverSkipped, out.Summary.LastFailureCode, out.Summary.LastFailureReason, out.Summary.LastFailureClass, out.Summary.LastReceiverExit, out.Summary.AuthState) {
		return Diagnostics{}, errors.New("invalid diagnostics")
	}
	return out, nil
}
func (s *Store) loadDiagnostics(key string) (Diagnostics, error) {
	if !diagnosticKey.MatchString(key) {
		return Diagnostics{}, errors.New("invalid diagnostic key")
	}
	legacy, _ := readDiagnostics(filepath.Join(s.dir, "diagnostics", key+".json"), false)
	out, err := readDiagnostics(filepath.Join(s.dir, "diagnostics", "v2", key+".json"), true)
	if legacy.Summary.LastRenewSuccess.After(out.Summary.LastRenewSuccess) {
		out.Summary.LastRenewSuccess = legacy.Summary.LastRenewSuccess
	}
	if legacy.Summary.LastFailureAt.After(out.Summary.LastFailureAt) {
		out.Summary.LastFailureAt = legacy.Summary.LastFailureAt
		out.Summary.LastFailureReason = legacy.Summary.LastFailureReason
		out.Summary.LastFailureCode = legacy.Summary.LastFailureCode
		out.Summary.LastFailureClass = legacy.Summary.LastFailureClass
	}
	if out.Summary.HandoverSkipped == "" {
		out.Summary.HandoverSkipped = legacy.Summary.HandoverSkipped
	}
	if out.Summary.LastReceiverExit == "" {
		out.Summary.LastReceiverExit = legacy.Summary.LastReceiverExit
	}
	if out.Summary.AuthState == "" {
		out.Summary.AuthState = legacy.Summary.AuthState
	}
	out.Events = append(legacy.Events, out.Events...)
	sort.SliceStable(out.Events, func(i, j int) bool { return out.Events[i].At.Before(out.Events[j].At) })
	if len(out.Events) > DiagnosticEventLimit {
		out.Events = out.Events[len(out.Events)-DiagnosticEventLimit:]
	}
	for len(out.Events) > 0 {
		raw, marshalErr := json.Marshal(out)
		if marshalErr != nil || len(raw) <= DiagnosticByteLimit {
			break
		}
		out.Events = out.Events[1:]
	}
	return out, err
}
func (s *Store) LoadDiagnostics(key string) (Diagnostics, error) {
	diagnosticsMu.Lock()
	defer diagnosticsMu.Unlock()
	out, err := s.loadDiagnostics(key)
	s.mergeDiagnosticSuccess(key, &out)
	return out, err
}

func (s *Store) mergeDiagnosticSuccess(key string, out *Diagnostics) {
	if latest := s.diagnosticRenewals[key].event.At; latest.After(out.Summary.LastRenewSuccess) {
		out.Summary.LastRenewSuccess = latest
	}
}

// RecordDiagnostic atomically replaces a bounded recent-event file using the
// same 0600 writer as credentials. Successful heartbeats update memory every
// time but persist at most once a minute; every other event persists immediately.
func (s *Store) RecordDiagnostic(key string, e DiagnosticEvent) error {
	return s.recordDiagnosticAt(key, e, time.Now())
}

func (s *Store) recordDiagnosticAt(key string, e DiagnosticEvent, now time.Time) error {
	if !diagnosticKey.MatchString(key) || !validDiagnostic(e) {
		return errors.New("invalid diagnostic event")
	}
	diagnosticsMu.Lock()
	defer diagnosticsMu.Unlock()
	return s.recordDiagnosticLocked(key, e, now, false)
}

// Caller holds diagnosticsMu. The monotonic write clock is separate from the
// event timestamp so a wall-clock adjustment cannot disable coalescing.
func (s *Store) recordDiagnosticLocked(key string, e DiagnosticEvent, now time.Time, flush bool) error {
	if e.Event == "renew-success" {
		if s.diagnosticRenewals == nil {
			s.diagnosticRenewals = make(map[string]diagnosticRenewal)
		}
		pending := s.diagnosticRenewals[key]
		if e.At.After(pending.event.At) {
			pending.event = e
		}
		pending.pending = true
		s.diagnosticRenewals[key] = pending
		if !flush && !pending.writtenAt.IsZero() && now.Sub(pending.writtenAt) < diagnosticSuccessInterval {
			return nil
		}
	}
	dir := filepath.Join(s.dir, "diagnostics")
	if !diagnosticDirectory(dir) || !diagnosticDirectory(filepath.Join(dir, "v2")) || !safeLockPath(filepath.Join(dir, "v2", key+".lock")) {
		return errors.New("diagnostics unavailable")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("diagnostics unavailable")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return errors.New("diagnostics unavailable")
	}
	v2 := filepath.Join(dir, "v2")
	if err := os.MkdirAll(v2, 0700); err != nil {
		return errors.New("diagnostics unavailable")
	}
	if err := os.Chmod(v2, 0700); err != nil {
		return errors.New("diagnostics unavailable")
	}
	release, err := lockFile(filepath.Join(v2, key+".lock"))
	if err != nil {
		return errors.New("diagnostics busy")
	}
	defer release()
	out, err := readDiagnostics(filepath.Join(v2, key+".json"), true)
	if err != nil {
		return err
	}
	if out.Format > 2 {
		return errors.New("newer diagnostics are read-only")
	}
	out.Format = 2
	if e.Event == "device-identity" {
		if s.deviceDiagnosticCauses == nil {
			s.deviceDiagnosticCauses = make(map[string]string)
		}
		if s.deviceDiagnosticCauses[key] == e.Reason {
			return nil
		}
		for i := len(out.Events) - 1; i >= 0; i-- {
			if out.Events[i].Event == e.Event {
				if out.Events[i].Reason == e.Reason {
					s.deviceDiagnosticCauses[key] = e.Reason
					return nil
				}
				break
			}
		}
	}
	switch e.Event {
	case "handover-skipped":
		out.Summary.HandoverSkipped = e.Reason
	case "renew-success":
		if e.At.After(out.Summary.LastRenewSuccess) {
			out.Summary.LastRenewSuccess = e.At
		}
	case "renew-failed", "receive-failed", "sink-failed", "reconnect-failed", "probe-failed":
		out.Summary.LastFailureAt = e.At
		out.Summary.LastFailureReason = e.Event
		out.Summary.LastFailureClass = e.ErrorClass
		out.Summary.LastFailureCode = e.Reason
	case "receiver-exit":
		out.Summary.LastReceiverExit = e.Reason
	case "auth-transition":
		out.Summary.AuthState = e.AuthState
	}
	s.mergeDiagnosticSuccess(key, &out)
	if e.Event != "renew-success" {
		out.Events = append(out.Events, e)
	}
	if len(out.Events) > DiagnosticEventLimit {
		out.Events = out.Events[len(out.Events)-DiagnosticEventLimit:]
	}
	for {
		raw, err := json.Marshal(out)
		if err != nil {
			return errors.New("diagnostics unavailable")
		}
		if len(raw) <= DiagnosticByteLimit {
			if err := writeFile(filepath.Join(v2, key+".json"), raw); err != nil {
				return errors.New("diagnostics unavailable")
			}
			if e.Event == "device-identity" {
				s.deviceDiagnosticCauses[key] = e.Reason
			}
			if pending, ok := s.diagnosticRenewals[key]; ok {
				pending.pending = false
				pending.writtenAt = now
				s.diagnosticRenewals[key] = pending
			}
			return nil
		}
		if len(out.Events) == 0 {
			return errors.New("diagnostics exceed limit")
		}
		out.Events = out.Events[1:]
	}
}

// FlushDiagnostics persists pending success summaries after the owner's writers
// have stopped. A failed write stays pending so a later flush can retry.
func (s *Store) FlushDiagnostics() error {
	diagnosticsMu.Lock()
	defer diagnosticsMu.Unlock()
	var result error
	for key, pending := range s.diagnosticRenewals {
		if pending.pending {
			result = errors.Join(result, s.recordDiagnosticLocked(key, pending.event, time.Now(), true))
		}
	}
	return result
}
