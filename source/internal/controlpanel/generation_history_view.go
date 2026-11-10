package controlpanel

import "net/url"

const (
	historyUnknownNotice  = "프로젝트 세대를 확인할 수 없는 기록입니다. 현재 프로젝트의 기록과 구분해 표시합니다."
	historyPreviousNotice = "현재 프로젝트와 다른 세대의 기록입니다."
	historyMixedNotice    = "현재 세대에 속하지 않거나 세대를 확인할 수 없는 기록입니다."
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
func historyLabel(generation string) string {
	if generation == "" {
		return "이전 기록(세대 불명)"
	}
	return "이전 세대"
}
func historyNotice(unknown, previous bool) string {
	if unknown && previous {
		return historyMixedNotice
	}
	if unknown {
		return historyUnknownNotice
	}
	if previous {
		return historyPreviousNotice
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
