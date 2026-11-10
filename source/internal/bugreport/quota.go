package bugreport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"time"
)

// Quota receipts are operational metadata, independent of the report retention
// window. They are committed in the same SQLite transaction as the report.
// Only hashes of project/registration selectors and acceptance times are stored.
type quotaEvent struct {
	RegistrationHash string    `json:"registrationHash"`
	At               time.Time `json:"at"`
}
type quotaState struct {
	Events []quotaEvent `json:"events"`
}

func quotaHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func reportScope(project, generation string) string { return project + "\x00" + generation }
func quotaKey(project string) string                { return "quota-" + quotaHash(project) }
func validQuota(raw []byte) bool {
	var quota quotaState
	if json.Unmarshal(raw, &quota) != nil || len(quota.Events) > MaxReports {
		return false
	}
	for _, event := range quota.Events {
		decoded, err := hex.DecodeString(event.RegistrationHash)
		if err != nil || len(decoded) != sha256.Size || event.At.IsZero() {
			return false
		}
	}
	return true
}
func (s *Store) quotaLocked(project, generation string, rows []Report, now time.Time) (quotaState, error) {
	raw, err := s.db.Get(quotaKey(reportScope(project, generation)))
	quota := quotaState{Events: []quotaEvent{}}
	if errors.Is(err, fs.ErrNotExist) {
		// Upgrade an existing report-only store without resetting active limits.
		for _, r := range rows {
			if r.Reporter.Project == Redact(project) && r.Reporter.ProjectGenerationID == generation && now.Sub(r.ReceivedAt) < 24*time.Hour {
				quota.Events = append(quota.Events, quotaEvent{quotaHash(r.Reporter.RegistrationID), r.ReceivedAt})
			}
		}
	} else if err != nil {
		return quota, errors.New("bug report quota unavailable")
	} else {
		if !validQuota(raw) {
			return quota, errors.New("bug report quota invalid")
		}
		if json.Unmarshal(raw, &quota) != nil {
			return quota, errors.New("bug report quota invalid")
		}
	}
	kept := quota.Events[:0]
	for _, event := range quota.Events {
		if now.Sub(event.At) < 24*time.Hour {
			kept = append(kept, event)
		}
	}
	quota.Events = kept
	return quota, nil
}
