package controlpanel

import (
	"errors"
	"net/http"

	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
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
	admin := i18n.T(pageData.Lang, "bug.unknown_admin")
	if u, ok := p.store.User(d.Actor); ok {
		admin = u.Username
	}
	v.Absent = true
	v.DeletionRequested = d.Phase != "deleted"
	v.DeletionAt, v.DeletionAtLabel, v.DeletionActor = reportTime(d.DeletedAt), reportTimeLabel(d.DeletedAt), admin
	v.Report = bugReportRow{Report: bugreport.Report{ID: id, Input: bugreport.Input{Title: i18n.T(pageData.Lang, "bug.title_fallback")}}}
	v.ListURL = reportURL("/admin/bug-reports", v, v.Page, "")
	pageData.View = "bug-report-detail"
	pageData.BugReports = v
	status := http.StatusGone
	if v.DeletionRequested {
		status = http.StatusNotFound
	}
	p.render(w, nil, status, pageData) // pageData.Lang is set by the caller
	return true
}
