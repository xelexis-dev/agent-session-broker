package controlpanel

import (
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/bugreport"
)

// bugReportStore is shared by the request-scoped production store and test fakes.
type bugReportStore interface {
	List(bugreport.Filter, int) ([]bugreport.Report, int, error)
	Get(string) (bugreport.Report, error)
	SetResolution(string, string, string, bugreport.Actor, string) (bugreport.Report, bool, error)
}

type historyBugReportStore interface {
	ListFor(string, bugreport.Filter, int) ([]bugreport.Report, int, error)
	GetFor(string, string, string) (bugreport.Report, error)
}

type bugReportRow struct {
	bugreport.Report
	GenerationLabel                                                                      string
	URL, SeverityLabel, SeverityClass, StatusLabel, StatusClass, ClientVersion, Received string
}
type bugReportAction struct {
	State, Label string
	Primary      bool
}
type bugReportChange struct {
	At, AtLabel, From, To, Actor, Session, Note string
	Agent                                       bool
}

// Kind marks the fields the template lays out differently; it never depends on the label text.
type bugReportField struct{ Label, Value, Kind string }
type bugReportEvent struct{ At, AtLabel, Event, Reason string }
type bugReportView struct {
	Rows                                              []bugReportRow
	Report                                            bugReportRow
	Status, Severity                                  string
	Bucket, Notice, CurrentURL, HistoryURL, ResetURL  string
	History                                           bool
	Page, Pages, Total                                int
	Filtered                                          bool
	PreviousURL, NextURL, ListURL                     string
	Fields, Reporter, Diagnostics                     []bugReportField
	Events                                            []bugReportEvent
	HasDiagnostics                                    bool
	ChangeLabel, ChangeActor, ChangeAt, ChangeAtLabel string
	Actions                                           []bugReportAction
	Changes                                           []bugReportChange
	Note, DetailURL, SuccessNotice                    string
	Conflict                                          bool
	DeleteConflict                                    bool
	DeleteURL                                         string
	CanDelete                                         bool
	Absent, DeletionRequested                         bool
	DeletionAt, DeletionAtLabel, DeletionActor        string
}

func reportValue(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
func reportTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
func reportTimeLabel(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

var reportSeverities = map[string]string{"critical": "bug.severity.critical", "high": "bug.severity.high", "medium": "bug.severity.medium", "low": "bug.severity.low"}

func reportRow(language string, r bugreport.Report) bugReportRow {
	row := bugReportRow{Report: r, SeverityClass: map[string]string{"critical": "bug-severity-critical", "high": "bug-severity-high"}[r.Severity], ClientVersion: "—", Received: reportTime(r.ReceivedAt)}
	row.SeverityLabel = "—"
	if key, ok := reportSeverities[r.Severity]; ok {
		row.SeverityLabel = i18n.T(language, key)
	}
	row.StatusLabel, row.StatusClass = reportStateLabel(language, r.State()), reportStateClass(r.State())
	if r.Client != nil {
		row.ClientVersion = reportValue(r.Client.ClientVersion)
	}
	return row
}

// Only typed filters/page survive links. No caller-supplied return URL is used.
func reportQuery(v url.Values) (bugReportView, error) {
	out := bugReportView{Status: "open", Page: 1}
	bucket, ok := historyBucket(v)
	if !ok {
		return out, bugreport.ErrInvalid
	}
	out.Bucket, out.History = bucket, bucket != "current"
	for _, key := range []string{"status", "severity", "page", "id"} {
		if len(v[key]) > 1 {
			return out, bugreport.ErrInvalid
		}
	}
	if values, ok := v["status"]; ok && len(values) > 0 {
		out.Status = values[0]
	}
	if out.Status != "open" && out.Status != "resolved" && out.Status != "ignored" && out.Status != "closed" && out.Status != "all" {
		return out, bugreport.ErrInvalid
	}
	out.Severity = v.Get("severity")
	switch out.Severity {
	case "", "low", "medium", "high", "critical":
	default:
		return out, bugreport.ErrInvalid
	}
	if value := v.Get("page"); value != "" {
		number, err := strconv.Atoi(value)
		if err != nil {
			return out, bugreport.ErrInvalid
		}
		out.Page = max(1, number)
	}
	out.Filtered = out.Status != "open" || out.Severity != ""
	return out, nil
}
func reportURL(path string, v bugReportView, page int, id string) string {
	q := url.Values{"status": {v.Status}, "page": {strconv.Itoa(page)}}
	if v.Bucket != "" && v.Bucket != "current" {
		q.Set("bucket", v.Bucket)
	}
	if v.Severity != "" {
		q.Set("severity", v.Severity)
	}
	if id != "" {
		q.Set("id", id)
	}
	return path + "?" + q.Encode()
}
func (p *panel) reportStore() bugReportStore {
	if p.bugReports != nil {
		return p.bugReports
	}
	if backend, ok := p.backend.(interface{ BugReports() *bugreport.Store }); ok {
		if store := backend.BugReports(); store != nil {
			return store
		}
	}
	return nil
}
func (p *panel) bugReportsHTTP(w http.ResponseWriter, r *http.Request, session loginSession) {
	user, ok := p.store.User(session.userID)
	if !ok || !user.Enabled || user.Role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	method := http.MethodGet
	if r.URL.Path == "/admin/bug-reports/status" || r.URL.Path == "/admin/bug-reports/delete" {
		method = http.MethodPost
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, "invalid query", 400)
		return
	}
	v, err := reportQuery(query)
	if err != nil {
		http.Error(w, "invalid filter", 400)
		return
	}
	store := p.reportStore()
	if store == nil {
		http.Error(w, bugreport.ErrUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	pageData := page{Lang: p.store.UserLanguage(user.ID), View: "bug-reports", User: user, CSRF: session.csrf, AdminView: true, Projects: p.store.ProjectsFor(user.ID)}
	renderStatus := http.StatusOK
	if r.URL.Path == "/admin/bug-reports/delete" {
		for key, values := range r.PostForm {
			if (key != "csrf" && key != "id" && key != "expectedVersion" && key != "confirm") || len(values) != 1 {
				http.Error(w, "invalid form", 400)
				return
			}
		}
		for _, key := range []string{"csrf", "id", "expectedVersion", "confirm"} {
			if len(r.PostForm[key]) != 1 || r.PostForm.Get(key) == "" {
				http.Error(w, "invalid form", 400)
				return
			}
		}
		if r.PostForm.Get("confirm") != "delete" {
			http.Error(w, "invalid form", 400)
			return
		}
		deleting, ok := store.(deletingBugReportStore)
		if !ok {
			reportHTTPError(w, bugreport.ErrUnavailable)
			return
		}
		id := r.PostForm.Get("id")
		_, err := deleting.Delete(user.ID, id, r.PostForm.Get("expectedVersion"))
		if err == nil || errors.Is(err, bugreport.ErrDeleteIncomplete) {
			code := "bug-report-deleted"
			if err != nil {
				code = "bug-report-delete-incomplete"
			}
			p.redirectNotice(w, r, reportURL("/admin/bug-reports", v, v.Page, ""), code, id)
			return
		}
		if errors.Is(err, bugreport.ErrNotFound) && p.reportDeletionHTTP(w, store, pageData, v, id) {
			return
		}
		if !errors.Is(err, bugreport.ErrConflict) {
			reportHTTPError(w, err)
			return
		}
		v.Conflict, v.DeleteConflict = true, true
		renderStatus = http.StatusConflict
		query.Set("id", id)
	}
	if r.URL.Path == "/admin/bug-reports/status" {
		if v.History {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		for key, values := range r.PostForm {
			if (key != "csrf" && key != "id" && key != "status" && key != "expectedVersion" && key != "note") || len(values) != 1 {
				http.Error(w, "invalid form", 400)
				return
			}
		}
		id, status := r.PostForm.Get("id"), r.PostForm.Get("status")
		if id == "" {
			http.Error(w, "invalid form", 400)
			return
		}
		if _, changed, err := store.SetResolution(id, status, r.PostForm.Get("note"), bugreport.Actor{UserID: user.ID, Source: "panel"}, r.PostForm.Get("expectedVersion")); err != nil {
			if !errors.Is(err, bugreport.ErrConflict) {
				reportHTTPError(w, err)
				return
			}
			renderStatus = http.StatusConflict
			v.Conflict = true
			v.Note = bugreport.Redact(r.PostForm.Get("note"))
			query.Set("id", id)
		} else {
			target := reportURL("/admin/bug-reports/detail", v, v.Page, id)
			if changed {
				p.redirectNotice(w, r, target, "bug-report-"+status)
			} else {
				http.Redirect(w, r, target, http.StatusSeeOther)
			}
			return
		}
	}
	listReports, getReport := store.List, store.Get
	if v.History {
		historyStore, ok := store.(historyBugReportStore)
		if !ok {
			http.Error(w, "history unavailable", http.StatusServiceUnavailable)
			return
		}
		listReports = func(f bugreport.Filter, page int) ([]bugreport.Report, int, error) {
			return historyStore.ListFor(user.ID, f, page)
		}
		getReport = func(id string) (bugreport.Report, error) { return historyStore.GetFor(user.ID, id, v.Bucket) }
	}
	if v.DeleteConflict {
		if hs, ok := store.(historyBugReportStore); ok {
			getReport = func(id string) (bugreport.Report, error) {
				for _, bucket := range []string{"current", "previous", "unknown"} {
					report, err := hs.GetFor(user.ID, id, bucket)
					if err == nil {
						return report, nil
					}
					if !errors.Is(err, bugreport.ErrNotFound) {
						return report, err
					}
				}
				return bugreport.Report{}, bugreport.ErrNotFound
			}
		}
	}
	toggle := v
	toggle.Bucket = "current"
	v.CurrentURL = reportURL("/admin/bug-reports", toggle, 1, "")
	toggle.Bucket = "history"
	v.HistoryURL = reportURL("/admin/bug-reports", toggle, 1, "")
	v.ResetURL = reportURL("/admin/bug-reports", bugReportView{Status: "open", Bucket: v.Bucket}, 1, "")
	unknown, previous := false, false
	if r.URL.Path == "/admin/bug-reports" {
		notice := p.takeNotice(w, r)
		if notice == noticeMessages["bug-report-delete-incomplete"] {
			pageData.Error = notice
		} else {
			v.SuccessNotice = notice
		}
		filter := bugreport.Filter{State: v.Status, Severity: v.Severity, Bucket: v.Bucket}
		rows, total, err := listReports(filter, v.Page)
		if err != nil {
			reportHTTPError(w, err)
			return
		}
		v.Total = total
		v.Pages = max(1, (total+bugreport.PageSize-1)/bugreport.PageSize)
		if v.Page > v.Pages {
			v.Page = v.Pages
			rows, _, err = listReports(filter, v.Page)
			if err != nil {
				reportHTTPError(w, err)
				return
			}
		}
		for _, r := range rows {
			row := reportRow(pageData.Lang, r)
			if v.History {
				row.GenerationLabel = historyLabel(pageData.Lang, r.Reporter.ProjectGenerationID)
				unknown = unknown || r.Reporter.ProjectGenerationID == ""
				previous = previous || r.Reporter.ProjectGenerationID != ""
			}
			row.URL = reportURL("/admin/bug-reports/detail", v, v.Page, r.ID)
			v.Rows = append(v.Rows, row)
		}
		if v.Page > 1 {
			v.PreviousURL = reportURL("/admin/bug-reports", v, v.Page-1, "")
		}
		if v.Page < v.Pages {
			v.NextURL = reportURL("/admin/bug-reports", v, v.Page+1, "")
		}
	} else {
		report, err := getReport(query.Get("id"))
		if err != nil {
			if errors.Is(err, bugreport.ErrNotFound) && p.reportDeletionHTTP(w, store, pageData, v, query.Get("id")) {
				return
			}
			reportHTTPError(w, err)
			return
		}
		pageData.View = "bug-report-detail"
		v.Report = reportRow(pageData.Lang, report)
		if v.History {
			v.Report.GenerationLabel = historyLabel(pageData.Lang, report.Reporter.ProjectGenerationID)
			unknown = report.Reporter.ProjectGenerationID == ""
			previous = !unknown
		}
		v.ListURL = reportURL("/admin/bug-reports", v, v.Page, "")
		v.DetailURL = reportURL("/admin/bug-reports/detail", v, v.Page, report.ID)
		_, v.CanDelete = store.(deletingBugReportStore)
		v.DeleteURL = reportURL("/admin/bug-reports/delete", v, v.Page, "")
		if !v.Conflict {
			v.SuccessNotice = localText(pageData.Lang, p.takeNotice(w, r))
			if v.SuccessNotice != "" && ((v.Status != "all" && v.Status != report.State()) || (v.Severity != "" && v.Severity != report.Severity)) {
				v.SuccessNotice += " " + i18n.T(pageData.Lang, "notice.not_in_current_filter")
			}
		}
		language := pageData.Lang
		v.Actions = reportActions(language, report.State())
		for i := len(report.StatusChanges) - 1; i >= max(0, len(report.StatusChanges)-20); i-- {
			change := report.StatusChanges[i]
			actorName := i18n.T(language, "bug.unknown_user")
			if actor, ok := p.store.User(change.Actor); ok {
				actorName = actor.Username
			}
			item := bugReportChange{At: reportTime(change.At), AtLabel: reportTimeLabel(change.At), From: reportStateLabel(language, change.From), To: reportStateLabel(language, change.To), Actor: actorName, Note: change.Note, Agent: change.Source == "mcp"}
			if change.Session != nil {
				item.Session = change.Session.Name
			}
			v.Changes = append(v.Changes, item)
		}
		for _, field := range []bugReportField{{Label: i18n.T(language, "bug.field.summary"), Value: report.Summary}, {Label: i18n.T(language, "bug.field.steps"), Value: report.Steps}, {Label: i18n.T(language, "bug.field.expected"), Value: report.Expected}, {Label: i18n.T(language, "bug.field.actual"), Value: report.Actual}, {Label: i18n.T(language, "bug.field.tool"), Value: report.Tool}} {
			if strings.TrimSpace(field.Value) != "" {
				v.Fields = append(v.Fields, field)
			}
		}
		v.Reporter = []bugReportField{{Label: i18n.T(language, "bug.project"), Value: reportValue(report.Reporter.Project)}, {Label: i18n.T(language, "bug.field.session_name"), Value: reportValue(report.Reporter.SessionName)}, {Label: i18n.T(language, "bug.field.registration"), Value: reportValue(report.Reporter.RegistrationID), Kind: "registration"}, {Label: i18n.T(language, "bug.field.harness"), Value: reportValue(report.Reporter.Harness)}, {Label: i18n.T(language, "bug.field.server_version"), Value: reportValue(report.ServerVersion), Kind: "server-version"}}
		if client := report.Client; client != nil {
			v.HasDiagnostics = true
			ready := i18n.T(language, "bug.diag.not_ready")
			if client.ReceiveReady {
				ready = i18n.T(language, "bug.diag.ready")
			}
			auth := reportValue(client.AuthState)
			if auth == "authorized" {
				auth = i18n.T(language, "bug.diag.authorized")
			}
			follow := reportValue(client.LastFollow)
			if strings.EqualFold(report.Reporter.Harness, "codex") {
				follow = "—"
			}
			v.Diagnostics = []bugReportField{{Label: i18n.T(language, "bug.diag.client_version"), Value: reportValue(client.ClientVersion)}, {Label: i18n.T(language, "bug.diag.server_version"), Value: reportValue(client.ServerVersion)}, {Label: i18n.T(language, "bug.diag.os"), Value: reportValue(client.OS) + " · " + reportValue(client.Arch)}, {Label: i18n.T(language, "bug.diag.auth"), Value: auth}, {Label: i18n.T(language, "bug.diag.receive"), Value: ready}, {Label: i18n.T(language, "bug.diag.follow"), Value: follow}}
			for _, event := range client.Events[:min(20, len(client.Events))] {
				v.Events = append(v.Events, bugReportEvent{At: reportTime(event.At), AtLabel: reportTimeLabel(event.At), Event: reportValue(event.Event), Reason: reportValue(event.ReasonClass)})
			}
		}
		if !report.StatusChangedAt.IsZero() {
			v.ChangeLabel = reportStateLabel(language, report.State())
			if report.Status == "open" {
				v.ChangeLabel = i18n.T(language, "bug.reopened")
			}
			v.ChangeActor = reportValue(report.StatusChangedBy)
			if actor, ok := p.store.User(report.StatusChangedBy); ok {
				v.ChangeActor = actor.Username
			}
			v.ChangeAt = reportTime(report.StatusChangedAt)
			v.ChangeAtLabel = reportTimeLabel(report.StatusChangedAt)
		}
		if len(v.Changes) > 0 {
			latest := v.Changes[0]
			v.ChangeLabel = latest.From + " → " + latest.To
			v.ChangeActor = latest.Actor
		}
	}
	v.Notice = historyNotice(pageData.Lang, unknown, previous)
	pageData.BugReports = v
	p.render(w, r, renderStatus, pageData)
}
func reportHTTPError(w http.ResponseWriter, err error) {
	if errors.Is(err, bugreport.ErrForbidden) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if errors.Is(err, bugreport.ErrUnavailable) {
		http.Error(w, bugreport.ErrUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	if errors.Is(err, bugreport.ErrNotFound) {
		http.NotFound(w, nil)
		return
	}
	if errors.Is(err, bugreport.ErrConflict) {
		http.Error(w, "Report changed; reload the latest version.", http.StatusConflict)
		return
	}
	if errors.Is(err, bugreport.ErrInvalid) {
		http.Error(w, "invalid report", 400)
		return
	}
	http.Error(w, "Could not process the bug report. Try again.", 500)
}

func reportStateLabel(language, state string) string {
	switch state {
	case "resolved":
		return i18n.T(language, "bug.state.resolved")
	case "ignored":
		return i18n.T(language, "bug.state.ignored")
	case "closed":
		return i18n.T(language, "bug.state.closed")
	default:
		return i18n.T(language, "bug.state.open")
	}
}
func reportStateClass(state string) string {
	switch state {
	case "resolved":
		return "state-badge--ready"
	case "ignored":
		return "state-badge--detached"
	case "closed":
		return "state-badge--unregistered"
	default:
		return ""
	}
}
func reportActions(language, state string) []bugReportAction {
	switch state {
	case "resolved":
		return []bugReportAction{{"open", i18n.T(language, "bug.action.reopen"), false}, {"ignored", i18n.T(language, "bug.action.ignore_change"), false}}
	case "ignored":
		return []bugReportAction{{"open", i18n.T(language, "bug.action.reopen"), false}, {"resolved", i18n.T(language, "bug.action.resolve_change"), false}}
	case "closed":
		return []bugReportAction{{"resolved", i18n.T(language, "bug.action.resolve"), true}, {"ignored", i18n.T(language, "bug.action.ignore"), false}, {"open", i18n.T(language, "bug.action.reopen"), false}}
	default:
		return []bugReportAction{{"resolved", i18n.T(language, "bug.action.resolve"), true}, {"ignored", i18n.T(language, "bug.action.ignore"), false}}
	}
}
