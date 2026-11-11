package controlpanel

import (
	"net/url"

	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
)

// Missing means the current view. An explicitly empty/duplicate value is invalid.
func historyBucket(q url.Values) (string, bool) {
	values, present := q["bucket"]
	if !present {
		return "current", true
	}
	if len(values) != 1 || values[0] == "" || !validRecordBucket(values[0]) {
		return "", false
	}
	return values[0], true
}
func historyLabel(language, generation string) string {
	if generation == "" {
		return i18n.T(language, "history.label_unknown")
	}
	return i18n.T(language, "history.label_previous")
}
func historyNotice(language string, unknown, previous bool) string {
	if unknown && previous {
		return i18n.T(language, "history.notice_mixed")
	}
	if unknown {
		return i18n.T(language, "history.notice_unknown")
	}
	if previous {
		return i18n.T(language, "history.notice_previous")
	}
	return ""
}
func auditHistoryURL(project, bucket string) string {
	q := url.Values{"bucket": {bucket}}
	if project != "" {
		q.Set("project", project)
	}
	return "/projects/audit?" + q.Encode()
}
