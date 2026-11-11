package controlpanel

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

const tokenScopeEditorInvalidMessage = "tokens.error_scope_invalid"
const tokenScopeHistoryMessage = "tokens.error_scope_history"
const tokenScopeReceiveAddedHelp = "tokens.client_0_17_2_resumes_receiving_automatically"
const tokenScopeReceiveRemovedHelp = "tokens.turning_off_message_receiving_also_prevents_this"

type scopeEditorKey struct{}
type scopeEditorChoice struct {
	Code, Label, Delta      string
	Held, Checked, Disabled bool
}
type scopeEditorGroup struct {
	Title string
	Rows  []scopeEditorChoice
}
type scopeEditorView struct {
	Current                                                 TokenScopeEdit
	Proposed                                                []string
	Groups                                                  []scopeEditorGroup
	Added, Removed                                          []string
	Step, Notice, CloseURL                                  string
	Error, HistoryError, Noop, ReceiveAdded, ReceiveRemoved bool
	Connected                                               int
	ConnectedKnown                                          bool
}

func scopeDifference(a, b []string) []string {
	var out []string
	for _, scope := range a {
		if !access.HasScope(b, scope) {
			out = append(out, scope)
		}
	}
	return out
}
func newScopeEditor(current TokenScopeEdit, proposed []string, step, notice string, isError, history bool) *scopeEditorView {
	baseline := access.DelegatedScopes(current.Scopes)
	proposed = slices.Clone(proposed)
	slices.Sort(proposed)
	e := &scopeEditorView{Current: current, Proposed: proposed, Step: step, Notice: notice, Error: isError, HistoryError: history, CloseURL: projectURL("settings", current.Project), Added: scopeDifference(proposed, baseline), Removed: scopeDifference(baseline, proposed)}
	e.Noop = len(e.Added)+len(e.Removed) == 0
	e.ReceiveAdded = access.HasScope(e.Added, access.MessageReceive)
	e.ReceiveRemoved = access.HasScope(e.Removed, access.MessageReceive)
	for _, group := range []struct {
		title string
		codes []string
	}{{"tokens.group_sessions", []string{access.SessionRead, access.SessionRename, access.SessionManage}}, {"tokens.group_messages", []string{access.MessageSend, access.MessageReceive}}, {"tokens.group_memory", []string{access.MemoryRead, access.MemoryWrite}}} {
		g := scopeEditorGroup{Title: group.title}
		for _, code := range group.codes {
			held, checked := access.HasScope(baseline, code), access.HasScope(proposed, code)
			delta := ""
			if held && !checked {
				delta = "tokens.delta_removed"
			} else if !held && checked {
				delta = "tokens.delta_added"
			}
			g.Rows = append(g.Rows, scopeEditorChoice{Code: code, Label: scopeLabelKey(code), Held: held, Checked: checked, Disabled: !current.CanWiden && !held, Delta: delta})
		}
		e.Groups = append(e.Groups, g)
	}
	return e
}
func scopeEditorFailure(err error) (int, string, bool) {
	switch {
	case errors.Is(err, errSettingsConflict):
		return 409, "tokens.error_scope_conflict", false
	case errors.Is(err, errTokenScopeHistory):
		return 400, tokenScopeHistoryMessage, true
	case errors.Is(err, ErrInvalid):
		return 400, tokenScopeEditorInvalidMessage, false
	case errors.Is(err, ErrForbidden):
		return 403, "tokens.error_scope_forbidden", false
	default:
		return 500, "tokens.error_scope_storage", false
	}
}
func (p *panel) renderScopeEditor(w http.ResponseWriter, r *http.Request, session loginSession, status int, e *scopeEditorView) {
	ctx := context.WithValue(r.Context(), scopeEditorKey{}, e)
	request := r.Clone(ctx)
	// Editor drafts must not alter the issuance form beneath the drawer.
	request.PostForm = nil
	request.Form = nil
	p.show(w, request, session, "settings", e.Current.Project, status, "", "")
}
func (p *panel) scopeEditorError(w http.ResponseWriter, r *http.Request, session loginSession, err error) {
	status, message, history := scopeEditorFailure(err)
	if status == 403 || status == 500 {
		http.Error(w, localText(p.requestLanguage(r, session.userID), message), status)
		return
	}
	current, loadErr := p.store.TokenScopeEditFor(session.userID, r.PostForm.Get("project"), r.PostForm.Get("id"))
	if loadErr != nil {
		code, msg, _ := scopeEditorFailure(loadErr)
		http.Error(w, localText(p.requestLanguage(r, session.userID), msg), code)
		return
	}
	p.renderScopeEditor(w, r, session, status, newScopeEditor(current, access.DelegatedScopes(current.Scopes), "select", message, true, history))
}
func (p *panel) scopeEditorForRequest(r *http.Request, session loginSession, project string) (*scopeEditorView, int, error) {
	incoming, _ := r.Context().Value(scopeEditorKey{}).(*scopeEditorView)
	id := ""
	if incoming != nil {
		id = incoming.Current.ParentID
	} else {
		values, ok := r.URL.Query()["edit"]
		if !ok {
			return nil, 0, nil
		}
		if len(values) != 1 || values[0] == "" {
			return nil, 400, ErrInvalid
		}
		id = values[0]
	}
	current, err := p.store.TokenScopeEditFor(session.userID, project, id)
	if err != nil {
		status, _, _ := scopeEditorFailure(err)
		return nil, status, err
	}
	if incoming == nil {
		return newScopeEditor(current, access.DelegatedScopes(current.Scopes), "select", "", false, false), 0, nil
	}
	checked, err := p.store.PreviewTokenScopesFor(session.userID, project, id, incoming.Proposed, []string{incoming.Current.Version})
	if err != nil {
		status, message, history := scopeEditorFailure(err)
		if status == 403 || status == 500 {
			return nil, status, err
		}
		return newScopeEditor(current, access.DelegatedScopes(current.Scopes), "select", message, true, history), status, nil
	}
	return newScopeEditor(checked, incoming.Proposed, incoming.Step, incoming.Notice, incoming.Error, incoming.HistoryError), 0, nil
}
func codesText(scopes []string) string {
	if len(scopes) == 0 {
		return ""
	}
	return strings.Join(scopes, ", ")
}
