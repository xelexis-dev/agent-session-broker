package controlpanel

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
)

const tokenScopeEditorInvalidMessage = "권한 변경 요청을 확인해 주세요. 일반 권한만 선택할 수 있습니다."
const tokenScopeHistoryMessage = "권한을 변경하지 않았습니다. 이전 자격 기록과 함께 저장할 수 없는 변경입니다. 토큰을 취소하고 새로 발급해 주세요."
const tokenScopeReceiveAddedHelp = "client 0.17.2는 수신을 자동으로 재개합니다. 이전 client는 재시작이 필요합니다."
const tokenScopeReceiveRemovedHelp = "메시지 수신을 끄면 이 토큰의 세션은 메시지를 보낼 수도 없습니다."

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
	labels := map[string]string{}
	for _, item := range access.ScopeCatalog() {
		labels[item.Scope] = item.Label
	}
	for _, group := range []struct {
		title string
		codes []string
	}{{"세션", []string{access.SessionRead, access.SessionRename, access.SessionManage}}, {"메시지", []string{access.MessageSend, access.MessageReceive}}, {"메모리", []string{access.MemoryRead, access.MemoryWrite}}} {
		g := scopeEditorGroup{Title: group.title}
		for _, code := range group.codes {
			held, checked := access.HasScope(baseline, code), access.HasScope(proposed, code)
			delta := ""
			if held && !checked {
				delta = "제거"
			} else if !held && checked {
				delta = "추가"
			}
			g.Rows = append(g.Rows, scopeEditorChoice{Code: code, Label: labels[code], Held: held, Checked: checked, Disabled: !current.CanWiden && !held, Delta: delta})
		}
		e.Groups = append(e.Groups, g)
	}
	return e
}
func scopeEditorFailure(err error) (int, string, bool) {
	switch {
	case errors.Is(err, errSettingsConflict):
		return 409, "권한을 변경하지 않았습니다. " + settingsConflictMessage, false
	case errors.Is(err, errTokenScopeHistory):
		return 400, tokenScopeHistoryMessage, true
	case errors.Is(err, ErrInvalid):
		return 400, tokenScopeEditorInvalidMessage, false
	case errors.Is(err, ErrForbidden):
		return 403, "권한을 변경할 수 없습니다.", false
	default:
		return 500, "권한을 불러오거나 저장하지 못했습니다.", false
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
		http.Error(w, message, status)
		return
	}
	current, loadErr := p.store.TokenScopeEditFor(session.userID, r.PostForm.Get("project"), r.PostForm.Get("id"))
	if loadErr != nil {
		code, msg, _ := scopeEditorFailure(loadErr)
		http.Error(w, msg, code)
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
		return "없음"
	}
	return strings.Join(scopes, ", ")
}
