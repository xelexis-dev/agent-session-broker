package controlpanel

import (
	"errors"
	"net/http"

	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
)

type deletingBugReportStore interface {
	Delete(actor, id, version string) (bugreport.Deletion, error)
	DeletionFor(actor, id string) (bugreport.Deletion, error)
}

// Only admins reach this helper; neither the record nor the response includes report text.
func (p *panel) reportDeletionHTTP(w http.ResponseWriter, store bugReportStore, pageData page, v bugReportView, id string) bool {
	deleting, ok := store.(deletingBugReportStore)
	if !ok {
		return false
	}
	d, err := deleting.DeletionFor(pageData.User.ID, id)
	if errors.Is(err, bugreport.ErrNotFound) {
		return false
	}
	if err != nil {
		reportHTTPError(w, err)
		return true
	}
	admin := "알 수 없는 관리자"
	if u, ok := p.store.User(d.Actor); ok {
		admin = u.Username
	}
	v.Absent = true
	v.DeletionRequested = d.Phase != "deleted"
	v.DeletionAt, v.DeletionAtLabel, v.DeletionActor = reportTime(d.DeletedAt), reportTimeLabel(d.DeletedAt), admin
	v.Report = bugReportRow{Report: bugreport.Report{ID: id, Input: bugreport.Input{Title: "버그 리포트"}}}
	v.ListURL = reportURL("/admin/bug-reports", v, v.Page, "")
	pageData.View = "bug-report-detail"
	pageData.BugReports = v
	status := http.StatusGone
	if v.DeletionRequested {
		status = http.StatusNotFound
	}
	p.render(w, status, pageData)
	return true
}
