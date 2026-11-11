package controlpanel

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/attachments"
	"github.com/xelexis-dev/agent-session-broker/source/internal/i18n"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Session is a presentation DTO; the panel has no harness/runtime dependency.
type Session struct {
	OwnerID         string // Server-recorded registration owner; never supplied by a panel request.
	RuntimeInfo     *protocol.RuntimeInfoSnapshot
	Activity        *protocol.ActivitySnapshot
	RegistrationID  string
	GrantID         string
	GrantOwner      string
	PendingNickname string
	ID              string
	SessionID       string
	Name            string
	Status          string
	Harness         string
	PID             int
	Ready           bool
	ClientID        string
	LastSeen        string
	ProbePending    bool
	Suspended       bool
	ErrorCode       string
}

var stateReasons = map[string]string{"suspended": "session.reason.suspended", "detached": "session.reason.detached", "server-restarted": "session.reason.server-restarted", "ingress-failed": "session.reason.ingress-failed", "ingress-expired": "session.reason.ingress-expired", "probe-unanswered": "session.reason.probe-unanswered", "identity-mismatch": "session.reason.identity-mismatch"}

// StateReason explains the runtime observation without changing its protocol status.
func (s Session) StateReason() i18n.Message {
	if s.GrantID != "" {
		if s.PendingNickname != "" {
			return i18n.M("session.reason.reservation_pending", "nickname", s.PendingNickname)
		}
		return i18n.M("session.reason.reservation")
	}
	if key, ok := stateReasons[s.ErrorCode]; ok {
		return i18n.M(key)
	}
	// An unknown code is shown as itself: a key missing from the catalog translates to the key.
	return i18n.M(s.ErrorCode)
}

type Backend interface {
	Sessions(context.Context, string) ([]Session, error)
	Detach(context.Context, string, string) error
}

//go:embed admin-api-panel.html token-scope-editor.html bug-reports.html direct-audit.html session-live.js templates.html login-return.js focus.css panel.css panel.js design.css theme.js shell.js select.css select.js tooltip.css tooltip.js switch.css tokens.css asset-manifest.json fonts/pretendard icons
var assets embed.FS

type loginSession struct {
	csrf     string
	expires  time.Time
	userID   string
	revision uint64
}
type panel struct {
	apiHandler        *http.ServeMux
	trustedProxies    []netip.Prefix
	transportLogs     map[string]time.Time
	bugReports        bugReportStore
	version           string
	deleteMu          sync.Mutex
	store             *Store
	backend           Backend
	next              http.Handler
	mu                sync.Mutex
	sessions          map[string]loginSession
	notices           map[string]pendingNotice
	tokenResults      map[string]issuedTokenResult
	tokenSubmissions  map[string]tokenSubmission
	announcements     map[string]*announcementSubmission
	suspensionWatches map[suspensionKey]*suspensionWatch
	codeAttempts      map[string]*attemptWindow
	attempts          int
	attemptWindow     time.Time
}
type memberView struct {
	ID, Role string
	Enabled  bool
}
type profileProject struct {
	Name string
	Role string
}

// profileProjects lists the user's explicit memberships by project name.
func profileProjects(projects []Project, userID string) []profileProject {
	labels := map[string]string{"owner": "Owner", "developer": "Developer", "viewer": "Viewer"}
	var result []profileProject
	for _, p := range projects {
		role := p.Members[userID]
		if role == "" {
			continue
		}
		if label := labels[role]; label != "" {
			role = label
		}
		result = append(result, profileProject{Name: p.Name, Role: role})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

type page struct {
	Lang                                          string // en or ko; resolved by render when empty
	ProfileLanguage                               string
	AdminAPIEnabled                               bool
	AdminAPILastChange                            *ScopeIssuanceChange
	APIHTTPRefused                                bool
	APIError                                      string
	APIIssueDisabled                              bool
	UserAPITokens                                 []UserTokenView
	IssuedAPIToken                                string
	ScopeEditor                                   *scopeEditorView
	AttachmentsEnabled                            bool
	AttachmentLastChange                          *ScopeIssuanceChange
	AttachmentDisk                                attachmentDiskView
	AttachmentMaxMiB                              int64
	AttachmentExpiryMinutes                       int64
	SettingsVersions                              map[string]string
	TokenSubmission                               string
	BugReports                                    bugReportView
	ProfileProjects                               []profileProject
	DirectAudit                                   directAuditView
	Version                                       string
	ScopeIssuanceReady                            bool
	ScopeIssuanceLastChange                       *ScopeIssuanceChange
	ScopeChoices                                  []scopeChoice
	AutoSelected                                  bool
	AutoCapValue, AutoDaysValue                   string
	ServerTime                                    string
	AnnouncementResults                           []AnnouncementResult
	AnnouncementComplete                          bool
	AnnouncementToken, AnnouncementMessage        string
	AnnouncementAccepted, AnnouncementFailed      int
	View, CSRF, Error, Project, Role, IssuedToken string
	LoginNext                                     string
	Code                                          string
	Device                                        *DeviceView
	Success                                       string
	User                                          User
	Projects                                      []Project
	Selected                                      Project
	Sessions                                      []Session
	ProjectTokens                                 []tokenView
	Members                                       []memberView
	Users                                         []User
	SelectedUser                                  User
	CanManage                                     map[string]bool
	CanEdit, CanIssue, CreateOpen                 bool
	UserCreateOpen                                bool
	MemberAddOpen                                 bool
	AllowHTTP                                     bool
	AdminView                                     bool
	Roles                                         map[string]string
	Summary                                       sessionSummary
	Owners                                        map[string]string
	Form                                          url.Values
}

func New(store *Store, backend Backend, next http.Handler, version ...string) http.Handler {
	return NewWithTrustedProxies(store, backend, next, nil, version...)
}

func NewWithTrustedProxies(store *Store, backend Backend, next http.Handler, proxies []netip.Prefix, version ...string) http.Handler {
	label := "dev"
	if len(version) > 0 && strings.TrimSpace(version[0]) != "" {
		label = strings.TrimSpace(version[0])
	}
	p := &panel{trustedProxies: append([]netip.Prefix(nil), proxies...), version: label, store: store, backend: backend, next: next, sessions: map[string]loginSession{}}
	p.apiHandler = p.apiMux()
	protection := http.NewCrossOriginProtection()
	protection.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			apiError(w, ErrForbidden)
		} else {
			if err := protection.Check(r); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
			}
		}
	}))
	return protection.Handler(http.HandlerFunc(p.serve))
}

func (p *panel) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		p.serveAPI(w, r)
		return
	}
	if r.URL.Path == "/healthz" {
		p.next.ServeHTTP(w, r)
		return
	}
	if r.URL.Path == "/oauth/transport-policy" {
		p.transportPolicy(w, r)
		return
	}
	if r.URL.Path == "/mcp" || r.URL.Path == "/attachments" || strings.HasPrefix(r.URL.Path, "/attachments/") {
		p.mcp(w, r)
		return
	}
	if (r.URL.Path == directPath || isDeviceRESTPath(r.URL.Path)) && p.oauthProjectRefused(w, r) {
		return
	}
	if r.URL.Path == directPath || r.URL.Path == capabilityPath {
		p.directHTTP(w, r)
		return
	}
	if isDeviceRESTPath(r.URL.Path) {
		p.deviceREST(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		if !serveAsset(w, r) {
			http.NotFound(w, r)
		}
		return
	}
	// Browsers fetch /favicon.ico on their own. Redirecting it to /setup or
	// /login would issue a new form cookie and break the open form's CSRF value.
	if r.URL.Path == "/favicon.ico" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodPost {
		formLimit := int64(16 << 10)
		if r.URL.Path == "/admin/announcement" {
			formLimit = 3*12000 + 4096
		} // URL-encoded UTF-8 expands to three bytes per input byte.
		r.Body = http.MaxBytesReader(w, r.Body, formLimit)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", 400)
			return
		}
	}
	if r.URL.Path == "/projects/sessions/live" {
		p.liveSessions(w, r)
		return
	}
	if r.URL.Path == "/setup" || r.URL.Path == "/login" {
		p.access(w, r)
		return
	}
	if !p.store.Ready() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	session, ok := p.session(r)
	if !ok {
		http.Redirect(w, r, loginLocation(r), http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodPost && !equal(session.csrf, r.PostForm.Get("csrf")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	switch r.URL.Path {
	case "/tokens/scopes":
		p.tokenScopesMutate(w, r, session)
	case "/admin/bug-reports", "/admin/bug-reports/detail", "/admin/bug-reports/status", "/admin/bug-reports/delete":
		p.bugReportsHTTP(w, r, session)
	case "/projects/audit":
		p.directAudit(w, r, session)
	case "/projects/members/suggestions":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p.memberSuggestions(w, r, session)
	case deviceVerificationPath:
		p.authCode(w, r, session)
	case "/", "/projects/settings", "/projects/sessions", "/admin/users", "/admin/users/detail", "/admin/settings", "/profile":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/" && r.URL.Query().Get("project") != "" {
			http.Redirect(w, r, projectURL("sessions", r.URL.Query().Get("project")), http.StatusSeeOther)
			return
		}
		view := "projects"
		if r.URL.Path == "/projects/settings" {
			view = "settings"
		}
		if r.URL.Path == "/projects/sessions" {
			view = "sessions"
		}
		if r.URL.Path == "/admin/users" {
			view = "users"
		}
		if r.URL.Path == "/admin/users/detail" {
			view = "user-detail"
		}
		if r.URL.Path == "/admin/settings" {
			view = "admin-settings"
		}
		if r.URL.Path == "/profile" {
			view = "profile"
		}
		p.show(w, r, session, view, r.URL.Query().Get("project"), 200, "", "")
	case "/projects/delete":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p.deleteProject(w, r, session)
	case "/admin/announcement":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p.announcementMutate(w, r, session)
	case "/admin/announcement/result":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p.announcementResult(w, r, session)
	case "/admin/attachments":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		p.attachmentPolicyMutate(w, r, session)
	case "/admin/settings/save":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p.instanceSettingsMutate(w, r, session)
	case "/admin/admin-api":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		p.adminAPIPolicyMutate(w, r, session)
	case "/profile/api-tokens":
		if r.Method == http.MethodGet {
			p.show(w, r, session, "api-tokens", "", 200, "", "")
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		p.profileAPITokensMutate(w, r, session)
	case "/profile/api-tokens/new":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		p.show(w, r, session, "api-token-new", "", 200, "", "")
	case "/profile/api-tokens/revoke", "/profile/api-tokens/revoke-all":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		p.profileAPITokensMutate(w, r, session)
	case "/admin/users/api-tokens/revoke", "/admin/users/api-tokens/revoke-all":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		p.adminUserAPITokensMutate(w, r, session)
	case "/profile/save", "/profile/password", "/profile/language":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p.profileMutate(w, r, session)
	case "/admin/scope-issuance":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p.scopeIssuanceMutate(w, r, session)
	case "/admin/http-policy":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p.instanceHTTPPolicyMutate(w, r, session)
	case "/projects/create", "/projects/save", "/projects/members", "/tokens/project", "/tokens/revoke", "/sessions/detach", "/sessions/suspend", "/sessions/unsuspend", "/admin/users/create", "/admin/users/update", "/logout":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p.mutate(w, r, session)
	default:
		http.NotFound(w, r)
	}
}

func (p *panel) transportPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.NotFound(w, r)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	projects := query["project"]
	if err != nil || len(query) != 1 || len(projects) != 1 || !projectName.MatchString(projects[0]) {
		http.NotFound(w, r)
		return
	}
	// This unauthenticated API exposes only the transport opt-in of enabled
	// projects; unavailable projects receive the same response.
	allowHTTP, available := p.store.TransportPolicy(projects[0])
	if !available {
		if p.writeProjectRefusal(w, r, projects[0]) {
			return
		}
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		Project   string `json:"project"`
		AllowHTTP bool   `json:"allow_http"`
	}{Project: projects[0], AllowHTTP: allowHTTP})
}

func projectURL(view, name string) string {
	return "/projects/" + view + "?project=" + url.QueryEscape(name)
}
func (p *panel) show(w http.ResponseWriter, r *http.Request, session loginSession, view, project string, status int, message, raw string, announcements ...*announcementSubmission) {
	user, ok := p.store.User(session.userID)
	if !ok || !user.Enabled {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	language := p.store.UserLanguage(session.userID)
	v := page{Lang: language, ProfileLanguage: language, ServerTime: activityTime(time.Now()), View: view, CSRF: session.csrf, User: user, Projects: nil, Error: message, IssuedToken: raw, Form: r.PostForm, CanManage: map[string]bool{}}
	if _, editing := r.Context().Value(scopeEditorKey{}).(*scopeEditorView); view == "settings" && (editing || r.URL.Query().Has("edit")) {
		v.Projects = p.store.projectSnapshotsFor(user.ID)
	} else {
		v.Projects = p.store.ProjectsFor(user.ID)
	}
	if len(announcements) > 0 {
		v.AnnouncementResults = announcements[0].results
		v.AnnouncementComplete = announcements[0].complete
		v.AnnouncementMessage = localText(language, announcements[0].message)
		if !announcements[0].complete {
			v.AnnouncementMessage = strings.TrimSpace(v.AnnouncementMessage + " " + i18n.T(language, "admin.announce.pending"))
		}
		for _, result := range v.AnnouncementResults {
			if result.Accepted {
				v.AnnouncementAccepted++
			} else {
				v.AnnouncementFailed++
			}
		}
	}
	v.AdminView = view == "users" || view == "user-detail" || view == "admin-settings"
	v.Roles = map[string]string{}
	for _, item := range v.Projects {
		v.Roles[item.Name] = p.store.ProjectRole(user.ID, item.Name)
	}
	if v.Form == nil {
		v.Form = url.Values{}
	}
	if view == "profile" || view == "api-tokens" || view == "api-token-new" {
		v.ProfileProjects = profileProjects(v.Projects, user.ID)
	}
	if view == "api-tokens" || view == "api-token-new" {
		v.APIHTTPRefused = p.plaintext(r) && !p.store.InstanceHTTPPolicy()
		v.APIError, _ = r.Context().Value(apiPanelErrorKey{}).(string)
		if view == "api-token-new" && v.APIHTTPRefused {
			v.APIError = "http"
		}
		v.APIIssueDisabled = v.APIError == "cap" || v.APIError == "rate" || v.APIError == "http"
		if view == "api-tokens" {
			v.UserAPITokens, _ = p.store.ListOwnUserTokens(user.ID)
			if status < 400 {
				v.IssuedAPIToken = p.takeIssuedTokenKind(r, session, user.ID, "user")
			}
		}
	}

	conflict := status == http.StatusConflict && message == settingsConflictMessage
	if view == "profile" && (r.Method == http.MethodGet || r.URL.Path == "/profile/password" || r.URL.Path == "/profile/language" || strings.HasPrefix(r.URL.Path, "/profile/api-tokens") || conflict) {
		v.Form = url.Values{"displayName": {user.DisplayName}, "email": {user.Email}}
	}
	if view == "admin-settings" {
		if user.Role != "admin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		v.AllowHTTP = p.store.InstanceHTTPPolicy()
		v.ScopeIssuanceReady, v.ScopeIssuanceLastChange = p.store.scopeIssuancePolicy()
		v.AnnouncementToken = p.issueAnnouncementToken(session)
	}
	if view == "user-detail" {
		if user.Role != "admin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		id := r.URL.Query().Get("id")
		if r.Method == http.MethodPost {
			id = r.PostForm.Get("id")
		}
		var exists bool
		v.SelectedUser, exists = p.store.User(id)
		if !exists {
			http.NotFound(w, r)
			return
		}
		v.UserAPITokens, _ = p.store.UserTokensFor(user.ID, id)
		v.Form = url.Values{"role": {v.SelectedUser.Role}}
		if v.SelectedUser.Enabled {
			v.Form.Set("enabled", "true")
		}
		if r.Method == http.MethodPost && !conflict {
			v.Form = url.Values{"role": {r.PostForm.Get("role")}, "enabled": {r.PostForm.Get("enabled")}}
		}
		if id == "admin" {
			v.Form.Set("role", "admin")
			v.Form.Set("enabled", "true")
		}
	}
	if view == "users" {
		v.UserCreateOpen = r.URL.Path == "/admin/users/create" && message != ""
		v.Form = url.Values{"username": {r.PostForm.Get("username")}, "role": {r.PostForm.Get("role")}}
		var err error
		v.Users, err = p.store.Users(user.ID)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	if view == "settings" || view == "sessions" {
		v.Project = project
		v.Role = p.store.ProjectRole(user.ID, project)
		if v.Role == "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		v.CanEdit = v.Role == "owner" || user.Role == "admin"
		v.CanIssue = v.CanEdit || v.Role == "developer"
		v.ScopeIssuanceReady = p.store.scopedIssuanceReady()
		v.ScopeChoices = projectScopeChoices(r.PostForm)
		v.AutoSelected = v.ScopeIssuanceReady && v.CanEdit && access.HasScope(r.PostForm["scope"], access.SessionRegisterAuto)
		v.AutoCapValue, v.AutoDaysValue = "8", "30"
		if value := r.PostForm.Get("auto_cap"); value != "" {
			v.AutoCapValue = value
			if value == "unlimited" {
				v.AutoCapValue = "0"
			}
		}
		if value := r.PostForm.Get("auto_days"); value != "" {
			v.AutoDaysValue = value
			if value == "unlimited" {
				v.AutoDaysValue = "0"
			}
		}
		for _, item := range v.Projects {
			if item.Name == project {
				v.Selected = item
			}
		}
		for id, role := range v.Selected.Members {
			member, exists := p.store.User(id)
			v.Members = append(v.Members, memberView{ID: id, Role: role, Enabled: exists && member.Enabled})
		}
		if v.Selected.Deleting {
			v.CanIssue = false
		}
		sort.Slice(v.Members, func(i, j int) bool { return v.Members[i].ID < v.Members[j].ID })
		if view == "settings" {
			sessions, err := p.projectSessions(r, project)
			if err != nil && v.Error == "" {
				v.Error = projectDataNoticeMessage(err, "error.sessions_unavailable_tokens")
			}
			v.ProjectTokens = projectTokenViews(v.Selected.Tokens, sessions, func(token SessionToken) bool { return v.CanEdit || token.OwnerID == user.ID })
			editor, editorStatus, editorErr := p.scopeEditorForRequest(r, session, project)
			if editorErr != nil {
				_, message, _ := scopeEditorFailure(editorErr)
				http.Error(w, message, editorStatus)
				return
			}
			if editorStatus != 0 {
				status = editorStatus
			}
			if editor != nil {
				editor.ConnectedKnown = err == nil
				for _, token := range v.ProjectTokens {
					if token.ID == editor.Current.ParentID {
						editor.Connected = token.Live
					}
				}
			}
			v.ScopeEditor = editor
			for i := range v.ProjectTokens {
				token := &v.ProjectTokens[i]
				_, err := p.store.TokenScopeEditFor(user.ID, project, token.ID)
				token.CanEditScopes = err == nil
				token.ScopeSelected = editor != nil && editor.Current.ParentID == token.ID
			}

		}
		if view == "sessions" {
			var err error
			v.Sessions, err = p.projectSessions(r, project)
			if err != nil {
				v.Error = projectDataNoticeMessage(err, "error.sessions_unavailable")
			}
			// Do not infer absence from a failed backend listing.
			if err == nil {
				reserved, reserveErr := p.store.reservedSessionGrants(project, v.Sessions)
				if reserveErr != nil {
					v.Error = "error.reservations_unavailable"
				} else {
					v.Sessions = append(v.Sessions, reserved...)
				}
			}
			owners := p.store.SessionOwners(project, v.Sessions...)
			v.Owners = map[string]string{}
			for _, item := range v.Sessions {
				v.CanManage[item.ID] = p.store.CanManageSession(user.ID, project, sessionIdentity(item), item.OwnerID)
				if item.GrantID != "" {
					v.CanManage[item.ID] = p.store.CanReleaseReservedGrant(user.ID, project, item.GrantID)
				}
				if item.GrantID != "" {
					v.Owners[item.ID] = item.GrantOwner
				}
				if owner, ok := owners[sessionIdentity(item)]; ok {
					v.Owners[item.ID] = owner
				}
			}
			v.Summary = summarizeSessions(v.Sessions)
		}
	}
	if message := p.takeNotice(w, r); status < 400 {
		v.Success = message
	}
	if view == "settings" && v.CanIssue {
		v.TokenSubmission = p.issueTokenSubmission(session, project)
	}
	if view == "settings" && status < 400 && v.CanIssue && raw == "" {
		v.IssuedToken = p.takeIssuedToken(r, session, project)
	}
	if v.IssuedToken != "" {
		v.Success = "notice.token_issued"
	}
	var attachmentVersion string
	var attachmentLast *attachmentSetting
	v.AttachmentsEnabled, attachmentVersion, attachmentLast = p.store.attachmentPolicySnapshot()
	// Same-value writes are no-ops, so every stored record is a real toggle.
	if attachmentLast != nil {
		v.AttachmentLastChange = &ScopeIssuanceChange{Actor: attachmentLast.Actor, At: attachmentLast.At, Old: policyBool(!bool(*attachmentLast.Enabled)), New: *attachmentLast.Enabled}
	}
	if view == "admin-settings" {
		v.AttachmentDisk = p.store.attachmentDiskView()
	}
	v.AttachmentMaxMiB = attachments.MaxFileBytes / (1 << 20)
	v.AttachmentExpiryMinutes = int64(attachments.Expiry / time.Minute)
	adminAPIEnabled, adminAPIVersion, lastAPIChange := p.store.adminAPIPolicySnapshot()
	v.AdminAPIEnabled = adminAPIEnabled
	v.AdminAPILastChange = lastAPIChange
	v.SettingsVersions = map[string]string{
		"admin-api":   adminAPIVersion,
		"project":     projectSettingsVersion(v.Selected),
		"user":        userSettingsVersion(v.SelectedUser),
		"profile":     profileSettingsVersion(v.User, v.ProfileLanguage),
		"http":        policySettingsVersion("http-policy", v.AllowHTTP),
		"scope":       policySettingsVersion("scope-issuance", v.ScopeIssuanceReady),
		"attachments": attachmentVersion,
	}
	// A validation error retains edited fields. Keep their original base version
	// if another save lands between validation and this rendering.
	if status == http.StatusBadRequest {
		keys := map[string]string{"/projects/save": "project", "/admin/users/update": "user", "/profile/save": "profile", "/admin/http-policy": "http", "/admin/scope-issuance": "scope"}
		if key := keys[r.URL.Path]; key != "" && len(r.PostForm["settings_version"]) == 1 {
			v.SettingsVersions[key] = r.PostForm.Get("settings_version")
		}
	}
	v.CreateOpen = view == "projects" && status >= 400
	v.MemberAddOpen = view == "settings" && status >= 400 && r.URL.Path == "/projects/members" && r.PostForm.Get("action") == "add"
	p.render(w, r, status, v)
}

// projectSessions reads the backend under the project operation lease so a
// concurrent delete cannot race the listing.
func (p *panel) projectSessions(r *http.Request, project string) ([]Session, error) {
	release, err := p.store.startProjectOperation(project, nil)
	if err != nil {
		return nil, err
	}
	defer release()
	return p.backend.Sessions(r.Context(), project)
}
func sessionIdentity(s Session) string {
	data, _ := json.Marshal([3]string{s.Harness, s.SessionID, s.ClientID})
	return string(data)
}
func (p *panel) mutate(w http.ResponseWriter, r *http.Request, session loginSession) {
	actor := session.userID
	path := r.URL.Path
	project := r.PostForm.Get("project")
	if path == "/projects/save" {
		project = r.PostForm.Get("name")
	}
	view := "sessions"
	if path == "/projects/save" || path == "/projects/members" || path == "/tokens/project" || path == "/tokens/revoke" {
		view = "settings"
	}
	if strings.HasPrefix(path, "/admin/users/") {
		view = "users"
	}
	if path == "/admin/users/update" {
		view = "user-detail"
	}
	if path == "/projects/create" {
		view = "projects"
	}
	fail := func(err error, message string) {
		if errors.Is(err, errScopeUpgradeRequired) {
			message = tokenScopeUpgradeMessage
		}
		if errors.Is(err, ErrInvalid) && path == "/tokens/project" {
			message = tokenScopeInvalidMessage
		}
		if errors.Is(err, ErrTokenCap) {
			message = tokenCapMessage
		}
		if errors.Is(err, ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if errors.Is(err, errSettingsConflict) {
			p.show(w, r, session, view, project, http.StatusConflict, settingsConflictMessage, "")
			return
		}
		p.show(w, r, session, view, project, 400, message, "")
	}
	forbidden := func() { http.Error(w, "forbidden", http.StatusForbidden) }
	if err := p.authorizeMutation(actor, path, project); err != nil {
		forbidden()
		return
	}
	switch path {
	case "/projects/create":
		name := r.PostForm.Get("name")
		if err := p.store.CreateProjectFor(actor, name); err != nil {
			fail(err, "error.project_create")
			return
		}
		p.redirectNotice(w, r, projectURL("settings", name), "project-created")
	case "/projects/save":
		mode := r.PostForm.Get("mode")
		if mode == "" {
			mode = "token"
		}
		if err := p.store.SaveProjectFor(actor, project, mode, r.PostForm.Get("enabled") == "true", r.PostForm["settings_version"]...); err != nil {
			fail(err, "error.project_save")
			return
		}
		p.redirectNotice(w, r, projectURL("settings", project), "project-saved")
	case "/projects/members":
		role := r.PostForm.Get("role")
		action := r.PostForm.Get("action")
		if action == "remove" {
			role = ""
		}
		changed, err := p.store.changeProjectMember(actor, project, r.PostForm.Get("username"), role, action)
		if err != nil {
			fail(err, "error.members_save")
			return
		}
		target := projectURL("settings", project)
		if p.store.ProjectRole(actor, project) == "" {
			target = "/"
		}
		notice := "members-saved"
		if !changed {
			notice = "members-unchanged"
		}
		p.redirectNotice(w, r, target, notice)
	case "/tokens/project":
		options, err := projectTokenForm(r.PostForm)
		if err != nil {
			fail(err, tokenScopeInvalidMessage)
			return
		}
		// Older forms and scripted clients omit the optional submission token.
		// A present field, including an empty one, must still be single-use.
		if values, present := r.PostForm["issuance_token"]; present && (len(values) != 1 || !p.takeTokenSubmission(session, project, values[0])) {
			p.show(w, r, session, "settings", project, http.StatusConflict, "error.token_form_expired", "")
			return
		}
		raw, err := p.store.IssueProjectTokenWithOptionsFor(actor, project, r.PostForm.Get("label"), options)
		if err != nil {
			fail(err, "error.token_issue")
			return
		}
		p.redirectIssuedToken(w, r, session, project, raw)
	case "/tokens/revoke":
		if err := p.store.RevokeTokenFor(actor, project, r.PostForm.Get("id")); err != nil {
			fail(err, "error.token_revoke")
			return
		}
		p.redirectNotice(w, r, projectURL("settings", project), "token-revoked")
	case "/sessions/detach", "/sessions/suspend", "/sessions/unsuspend":
		notice, err := p.manageSession(r.Context(), actor, project, path, r.PostForm.Get("id"), r.PostForm.Get("grant_id"), nil)
		if err != nil {
			var failure *sessionMutationError
			if errors.As(err, &failure) && failure.status == 409 {
				p.show(w, r, session, "sessions", project, 409, failure.message, "")
			} else if errors.As(err, &failure) {
				fail(failure.cause, failure.message)
			} else {
				fail(err, "error.session_select")
			}
			return
		}
		p.redirectNotice(w, r, projectURL("sessions", project), notice)
	case "/admin/users/create":
		if err := p.store.CreateUser(actor, r.PostForm.Get("username"), r.PostForm.Get("password"), r.PostForm.Get("role")); err != nil {
			fail(err, "error.user_create")
			return
		}
		p.redirectNotice(w, r, "/admin/users", "user-created")
	case "/admin/users/update":
		current, exists := p.store.User(r.PostForm.Get("id"))
		if !exists {
			http.NotFound(w, r)
			return
		}
		// Check before input validation too; the setter repeats under its write lock.
		if err := checkSettingsVersion(r.PostForm["settings_version"], userSettingsVersion(current)); err != nil {
			fail(err, "")
			return
		}
		// Both empty keeps the password; anything else must match before
		// anything is saved. equal treats an empty first value as unequal.
		if password, confirm := r.PostForm.Get("password"), r.PostForm.Get("password_confirm"); (password != "" || confirm != "") && !equal(password, confirm) {
			fail(ErrInvalid, "error.new_password_mismatch")
			return
		}
		if err := p.store.UpdateUser(actor, r.PostForm.Get("id"), r.PostForm.Get("role"), r.PostForm.Get("enabled") == "true", r.PostForm.Get("password"), r.PostForm["settings_version"]...); err != nil {
			fail(err, "error.user_save")
			return
		}
		if updated, ok := p.store.User(actor); !ok || !updated.Enabled || updated.Revision != session.revision {
			p.redirectNotice(w, r, "/login", "user-saved-relogin")
			return
		}
		p.redirectNotice(w, r, "/admin/users/detail?id="+url.QueryEscape(r.PostForm.Get("id")), "user-saved")
	case "/logout":
		if cookie, err := r.Cookie("asb_admin"); err == nil {
			p.mu.Lock()
			delete(p.sessions, digest(cookie.Value))
			p.mu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: "asb_admin", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: p.secure(r), SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

func (p *panel) hasProject(name string) bool {
	for _, project := range p.store.Projects() {
		if project.Name == name {
			return true
		}
	}
	return false
}

func (p *panel) mcp(w http.ResponseWriter, r *http.Request) {
	p.authorizedProject(w, r, p.next)
}

// HTTP attachments and MCP share one bearer, transport and cancellation boundary.
func (p *panel) authorizedProject(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if !p.store.Ready() {
		http.Error(w, "administrator setup required", http.StatusServiceUnavailable)
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	names := q["project"]
	if err != nil || len(names) != 1 || !projectName.MatchString(names[0]) {
		http.Error(w, "invalid project", 400)
		return
	}
	if !p.hasProject(names[0]) {
		p.writeProjectRefusal(w, r, names[0])
		return
	}
	// Reject already-disabled plaintext traffic before inspecting credentials. The
	// post-authorization check below closes the gap with a concurrent toggle.
	if p.plaintext(r) && !p.store.AllowHTTP(names[0]) {
		if _, available := p.store.TransportPolicy(names[0]); !available && p.writeProjectRefusal(w, r, names[0]) {
			return
		}
		p.denyTransport(w, r, names[0])
		return
	}
	token := ""
	if len(r.Header.Values("Authorization")) == 1 {
		scheme, value, found := strings.Cut(r.Header.Get("Authorization"), " ")
		if found && strings.EqualFold(scheme, "Bearer") {
			token = value
		}
	}
	grant, changes, allowed := p.store.authorize(names[0], token)
	if !allowed {
		if p.writeProjectRefusal(w, r, names[0]) {
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="asb-project"`)
		http.Error(w, "project authorization required", http.StatusUnauthorized)
		return
	}
	// Authorization captures a generation before checking plaintext transport.
	// A toggle that races after it is therefore a retryable policy change, while
	// a request admitted after HTTP was disabled is rejected below.
	if p.plaintext(r) {
		select {
		case <-changes:
			p.writeChangedAuthorizationForRequest(w, r, names[0], token)
			return
		default:
		}
		if !p.store.AllowHTTP(names[0]) {
			p.denyTransport(w, r, names[0])
			return
		}
	}
	release, startErr := p.store.startProjectOperation(names[0], changes)
	if startErr != nil {
		p.writeChangedAuthorizationForRequest(w, r, names[0], token)
		return
	}
	defer release()
	grant.CancelSuspendedRequests = func(nickname string) { p.cancelSuspendedRequests(names[0], nickname) }
	suspension, releaseSuspension := p.watchSuspension(names[0], grant.Nickname)
	defer releaseSuspension()
	if policy, ok := p.backend.(interface {
		SessionSuspended(context.Context, string, string) (bool, error)
	}); ok {
		suspended, err := policy.SessionSuspended(r.Context(), names[0], grant.Nickname)
		if err != nil {
			http.Error(w, projectDataErrorMessage(err, "session policy unavailable"), http.StatusServiceUnavailable)
			return
		}
		if suspended {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "session suspended", http.StatusLocked)
			return
		}
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if grant.TokenID != "" {
		ctx = access.WithGrant(ctx, grant)
	}
	select {
	case <-suspension:
		w.Header().Set("Retry-After", "10")
		http.Error(w, "session suspended", http.StatusLocked)
		return
	default:
	}
	select {
	case <-changes:
		p.writeChangedAuthorizationForRequest(w, r, names[0], token)
		return
	default:
	}
	// A session credential's own revocation or absolute expiry ends a long
	// stream even when no other project policy changes.
	var expiry <-chan time.Time
	if !grant.ExpiresAt.IsZero() {
		timer := time.NewTimer(time.Until(grant.ExpiresAt))
		defer timer.Stop()
		expiry = timer.C
	}
	go func() {
		select {
		case <-suspension:
			cancel()
		case <-changes:
			cancel()
		case <-grant.Revoked:
			cancel()
		case <-expiry:
			cancel()
		case <-ctx.Done():
		}
	}()
	next.ServeHTTP(w, r.WithContext(ctx))
}

// writeChangedAuthorization distinguishes durable credential revocation from a
// policy-generation change such as the HTTP opt-in. A valid credential may
// retry after refreshing its transport policy; an invalid one must reauthorize.
func (p *panel) writeChangedAuthorization(w http.ResponseWriter, project, token string) {
	if p.store.Authorize(project, token) {
		http.Error(w, "project policy changed; retry", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="asb-project"`)
	http.Error(w, "project authorization changed", http.StatusUnauthorized)
}

func (p *panel) session(r *http.Request) (loginSession, bool) {
	cookie, err := r.Cookie("asb_admin")
	if err != nil || len(cookie.Value) > 256 {
		return loginSession{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := digest(cookie.Value)
	s, ok := p.sessions[key]
	user, exists := p.store.User(s.userID)
	if ok && (!time.Now().Before(s.expires) || !exists || !user.Enabled || user.Revision != s.revision) {
		delete(p.sessions, key)
		return loginSession{}, false
	}
	return s, ok
}

func equal(a, b string) bool { return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func (p *panel) access(w http.ResponseWriter, r *http.Request) {
	setup := r.URL.Path == "/setup"
	if setup && p.store.Ready() {
		http.Error(w, "administrator already configured", http.StatusConflict)
		return
	}
	if !setup && !p.store.Ready() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	v := page{View: strings.TrimPrefix(r.URL.Path, "/"), Form: r.PostForm}
	if !setup {
		v.LoginNext = loginNext(r)
	}
	if v.Form == nil {
		v.Form = url.Values{}
	}
	if r.Method == http.MethodGet {
		v.Success = p.takeNotice(w, r)
		v.CSRF = rand.Text()
		http.SetCookie(w, &http.Cookie{Name: "asb_form", Value: v.CSRF, Path: "/", MaxAge: 600, HttpOnly: true, Secure: p.secure(r), SameSite: http.SameSiteStrictMode})
		p.render(w, r, 200, v)
		return
	}
	cookie, err := r.Cookie("asb_form")
	if err != nil || !equal(cookie.Value, r.PostForm.Get("csrf")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	v.CSRF = cookie.Value
	if !p.allowAttempt() {
		http.Error(w, i18n.T(p.requestLanguage(r, ""), "error.rate_limited"), http.StatusTooManyRequests)
		return
	}
	password := r.PostForm.Get("password")
	if setup {
		setupCode := r.PostForm.Get("setupCode")
		// Never reflect the setup proof, including when later password validation fails.
		v.Form.Del("setupCode")
		setupError := func(err error) {
			switch {
			case errors.Is(err, ErrAlreadySetup):
				http.Error(w, "administrator already configured", http.StatusConflict)
			case errors.Is(err, ErrSetupCode):
				v.Error = "error.setup_code"
				p.render(w, r, http.StatusForbidden, v)
			default:
				v.Error = "error.setup_password"
				p.render(w, r, http.StatusBadRequest, v)
			}
		}
		if err := p.store.checkSetupCode(setupCode); err != nil {
			setupError(err)
			return
		}
		if !equal(password, r.PostForm.Get("confirmPassword")) {
			v.Error = "error.password_mismatch"
			p.render(w, r, http.StatusBadRequest, v)
			return
		}
		if err := p.store.SetupWithCode(setupCode, password); err != nil {
			setupError(err)
			return
		}
		p.redirectNotice(w, r, "/login", "setup-saved")
		return
	}
	user, authenticated := p.store.AuthenticateUser(r.PostForm.Get("username"), password)
	if !authenticated {
		v.Error = "error.sign_in"
		p.render(w, r, 401, v)
		return
	}
	token := rand.Text() + rand.Text()
	p.mu.Lock()
	for key, s := range p.sessions {
		if !time.Now().Before(s.expires) {
			delete(p.sessions, key)
		}
	}
	if len(p.sessions) >= 128 {
		p.mu.Unlock()
		http.Error(w, "too many login sessions", http.StatusTooManyRequests)
		return
	}
	p.sessions[digest(token)] = loginSession{csrf: rand.Text(), expires: time.Now().Add(8 * time.Hour), userID: user.ID, revision: user.Revision}
	p.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "asb_admin", Value: token, Path: "/", MaxAge: 8 * 3600, HttpOnly: true, Secure: p.secure(r), SameSite: http.SameSiteStrictMode})
	p.setLanguageCookie(w, r, p.store.UserLanguage(user.ID))
	next := v.LoginNext
	if next == "" {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (p *panel) allowAttempt() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.attemptWindow) >= time.Minute {
		p.attemptWindow = time.Now()
		p.attempts = 0
	}
	if p.attempts >= 10 {
		return false
	}
	p.attempts++
	return true
}

func (p *panel) render(w http.ResponseWriter, r *http.Request, status int, v page) {
	v.Version = p.version
	if i18n.Normalize(v.Lang) == "" {
		v.Lang = p.requestLanguage(r, v.User.ID)
	}
	render(w, status, v)
}

func render(w http.ResponseWriter, status int, v page) {
	set := pageSets[v.Lang]
	if set == nil {
		v.Lang, set = i18n.English, pages
	}
	v.Error, v.Success = localText(v.Lang, v.Error), localText(v.Lang, v.Success)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = set.ExecuteTemplate(w, "page", v)
}

func (p *panel) writeChangedAuthorizationForRequest(w http.ResponseWriter, r *http.Request, project, token string) {
	if !p.store.Authorize(project, token) && p.writeProjectRefusal(w, r, project) {
		return
	}
	p.writeChangedAuthorization(w, project, token)
}
