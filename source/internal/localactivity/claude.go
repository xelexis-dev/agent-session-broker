package localactivity

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (o *Observer) observeClaude(b *readBudget, process Process, now time.Time) (string, time.Time, error) {
	if o.config.PID <= 0 {
		return "", time.Time{}, mismatch
	}
	f, err := o.openEvidence(filepath.Join("sessions", strconv.Itoa(o.config.PID)+".json"))
	if err != nil {
		return "", time.Time{}, err
	}
	defer f.close()
	data, err := b.readAt(f.file, 0, f.info.Size())
	if err != nil {
		return "", time.Time{}, err
	}
	var record struct {
		PID       int    `json:"pid"`
		SessionID string `json:"sessionId"`
		CWD       string `json:"cwd"`
		Kind      string `json:"kind"`
		Domain    string `json:"pidDomain"`
		Start     string `json:"procStart"`
		Status    string `json:"status"`
		Updated   int64  `json:"statusUpdatedAt"`
	}
	if json.Unmarshal(data, &record) != nil {
		return "", time.Time{}, malformed
	}
	if record.PID != o.config.PID || record.SessionID != o.config.Binding.SessionID || !samePath(record.CWD, o.config.CWD) || record.Kind != "interactive" || !strings.EqualFold(record.Domain, o.config.Platform+":"+o.config.Host) || process.RecordStart == "" || record.Start != process.RecordStart {
		return "", time.Time{}, mismatch
	}
	changed := time.UnixMilli(record.Updated).UTC()
	if record.Updated <= 0 || !trustedTime(changed, now) {
		return "", time.Time{}, malformed
	}
	if err = f.validate(); err != nil {
		return "", time.Time{}, err
	}
	switch record.Status {
	case "busy":
		if !now.Before(changed.Add(staleAfter)) {
			return "", time.Time{}, stale
		}
		return Working, changed, nil
	case "idle":
		return Idle, changed, nil
	default:
		return "", time.Time{}, malformed
	}
}
