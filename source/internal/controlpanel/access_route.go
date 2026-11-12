package controlpanel

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/accesslog"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"net/http"
	"net/netip"
	"path"
	"strings"
	"sync"
)

const (
	panelRouteRoot                         = "/"
	panelRouteAdminAdminApi                = "/admin/admin-api"
	panelRouteAdminAnnouncement            = "/admin/announcement"
	panelRouteAdminAnnouncementResult      = "/admin/announcement/result"
	panelRouteAdminAttachments             = "/admin/attachments"
	panelRouteAdminBugReports              = "/admin/bug-reports"
	panelRouteAdminBugReportsDelete        = "/admin/bug-reports/delete"
	panelRouteAdminBugReportsDetail        = "/admin/bug-reports/detail"
	panelRouteAdminBugReportsStatus        = "/admin/bug-reports/status"
	panelRouteAdminHttpPolicy              = "/admin/http-policy"
	panelRouteAdminScopeIssuance           = "/admin/scope-issuance"
	panelRouteAdminSettings                = "/admin/settings"
	panelRouteAdminSettingsSave            = "/admin/settings/save"
	panelRouteAdminUsers                   = "/admin/users"
	panelRouteAdminUsersApiTokensRevoke    = "/admin/users/api-tokens/revoke"
	panelRouteAdminUsersApiTokensRevokeAll = "/admin/users/api-tokens/revoke-all"
	panelRouteAdminUsersCreate             = "/admin/users/create"
	panelRouteAdminUsersDetail             = "/admin/users/detail"
	panelRouteAdminUsersUpdate             = "/admin/users/update"
	panelRouteAttachments                  = "/attachments"
	panelRouteFaviconIco                   = "/favicon.ico"
	panelRouteHealthz                      = "/healthz"
	panelRouteLogin                        = "/login"
	panelRouteLogout                       = "/logout"
	panelRouteMcp                          = "/mcp"
	panelRouteOauthTransportPolicy         = "/oauth/transport-policy"
	panelRouteProfile                      = "/profile"
	panelRouteProfileApiTokens             = "/profile/api-tokens"
	panelRouteProfileApiTokensNew          = "/profile/api-tokens/new"
	panelRouteProfileApiTokensRevoke       = "/profile/api-tokens/revoke"
	panelRouteProfileApiTokensRevokeAll    = "/profile/api-tokens/revoke-all"
	panelRouteProfileLanguage              = "/profile/language"
	panelRouteProfilePassword              = "/profile/password"
	panelRouteProfileSave                  = "/profile/save"
	panelRouteProjectsAudit                = "/projects/audit"
	panelRouteProjectsCreate               = "/projects/create"
	panelRouteProjectsDelete               = "/projects/delete"
	panelRouteProjectsMembers              = "/projects/members"
	panelRouteProjectsMembersSuggestions   = "/projects/members/suggestions"
	panelRouteProjectsSave                 = "/projects/save"
	panelRouteProjectsSessions             = "/projects/sessions"
	panelRouteProjectsSessionsLive         = "/projects/sessions/live"
	panelRouteProjectsSettings             = "/projects/settings"
	panelRouteSessionsDetach               = "/sessions/detach"
	panelRouteSessionsSuspend              = "/sessions/suspend"
	panelRouteSessionsUnsuspend            = "/sessions/unsuspend"
	panelRouteSetup                        = "/setup"
	panelRouteTokensProject                = "/tokens/project"
	panelRouteTokensRevoke                 = "/tokens/revoke"
	panelRouteTokensScopes                 = "/tokens/scopes"
)

// panelRoutes is shared with serve; path values are defined once.
var panelRoutes = []string{
	panelRouteRoot,
	panelRouteAdminAdminApi,
	panelRouteAdminAnnouncement,
	panelRouteAdminAnnouncementResult,
	panelRouteAdminAttachments,
	panelRouteAdminBugReports,
	panelRouteAdminBugReportsDelete,
	panelRouteAdminBugReportsDetail,
	panelRouteAdminBugReportsStatus,
	panelRouteAdminHttpPolicy,
	panelRouteAdminScopeIssuance,
	panelRouteAdminSettings,
	panelRouteAdminSettingsSave,
	panelRouteAdminUsers,
	panelRouteAdminUsersApiTokensRevoke,
	panelRouteAdminUsersApiTokensRevokeAll,
	panelRouteAdminUsersCreate,
	panelRouteAdminUsersDetail,
	panelRouteAdminUsersUpdate,
	panelRouteAttachments,
	panelRouteFaviconIco,
	panelRouteHealthz,
	panelRouteLogin,
	panelRouteLogout,
	panelRouteMcp,
	panelRouteOauthTransportPolicy,
	panelRouteProfile,
	panelRouteProfileApiTokens,
	panelRouteProfileApiTokensNew,
	panelRouteProfileApiTokensRevoke,
	panelRouteProfileApiTokensRevokeAll,
	panelRouteProfileLanguage,
	panelRouteProfilePassword,
	panelRouteProfileSave,
	panelRouteProjectsAudit,
	panelRouteProjectsCreate,
	panelRouteProjectsDelete,
	panelRouteProjectsMembers,
	panelRouteProjectsMembersSuggestions,
	panelRouteProjectsSave,
	panelRouteProjectsSessions,
	panelRouteProjectsSessionsLive,
	panelRouteProjectsSettings,
	panelRouteSessionsDetach,
	panelRouteSessionsSuspend,
	panelRouteSessionsUnsuspend,
	panelRouteSetup,
	panelRouteTokensProject,
	panelRouteTokensRevoke,
	panelRouteTokensScopes,
	directPath,
	capabilityPath,
	deviceVerificationPath,
	deviceAuthorizationPath,
	deviceTokenPath,
}

var accessRoutes []apiRoute
var panelRouteSet = map[string]string{}
var apiRouteSet = map[string]string{}

func init() {
	accessRoutes = apiRoutes
	for _, route := range panelRoutes {
		panelRouteSet[route] = route
	}
	serverdiag.RegisterRoutes(panelRoutes...)
	for _, route := range accessRoutes {
		serverdiag.RegisterRoutes(route.Path, route.Method+" "+route.Path)
		apiRouteSet[route.Path] = route.Path
	}
}

var accessAPIOnce sync.Once
var accessAPIMux *http.ServeMux

func staticAPIRoute(route string) string {
	for _, v := range accessRoutes {
		if route == v.Method+" "+v.Path {
			return v.Method + " " + v.Path
		}
	}
	return "/api/{unmatched}"
}

// AccessRoute returns only registered templates, never the original variable path.
func AccessRoute(r *http.Request) string {
	if canonical, ok := panelRouteSet[r.URL.Path]; ok {
		return canonical
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		accessAPIOnce.Do(func() {
			accessAPIMux = http.NewServeMux()
			seen := map[string]bool{}
			for _, v := range accessRoutes {
				if !seen[v.Path] {
					accessAPIMux.HandleFunc(v.Path, func(http.ResponseWriter, *http.Request) {})
					seen[v.Path] = true
				}
			}
		})
		if path.Clean(r.URL.Path) != r.URL.Path {
			return "/api/{unmatched}"
		}
		trial := *r
		url := *r.URL
		url.RawPath = ""
		trial.URL = &url
		trial.Method = "GET"
		_, pattern := accessAPIMux.Handler(&trial)
		if canonical, ok := apiRouteSet[pattern]; ok {
			return canonical
		}

		return "/api/{unmatched}"
	}
	if strings.HasPrefix(r.URL.Path, "/attachments/") {
		return "/attachments/{id}"
	}
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		return "/assets/{file}"
	}
	return "{unmatched}"
}

// AccessMetadata reuses the scheme decision without expanding authentication trust.
func AccessMetadata(r *http.Request, scheme, address []netip.Prefix, forwarded bool) accesslog.Metadata {
	p := panel{trustedProxies: scheme}
	return accesslog.Metadata{Route: AccessRoute(r), Address: accesslog.ClientAddress(r, forwarded, address), TLS: p.transport(r).secure}
}
