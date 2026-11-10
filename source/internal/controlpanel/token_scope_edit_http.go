package controlpanel

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"net/http"
)

func (p *panel) tokenScopesMutate(w http.ResponseWriter, r *http.Request, session loginSession) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", 405)
		return
	}
	for key, values := range r.PostForm {
		switch key {
		case "scope":
		case "project", "id", "settings_version", "csrf", "intent":
			if len(values) != 1 {
				p.scopeEditorError(w, r, session, ErrInvalid)
				return
			}
		default:
			p.scopeEditorError(w, r, session, ErrInvalid)
			return
		}
	}
	intent := r.PostForm.Get("intent")
	if intent == "" {
		intent = "apply"
	}
	if intent != "review" && intent != "back" && intent != "apply" {
		p.scopeEditorError(w, r, session, ErrInvalid)
		return
	}
	var current TokenScopeEdit
	var err error
	if intent == "apply" {
		current, err = p.store.EditTokenScopesFor(session.userID, r.PostForm.Get("project"), r.PostForm.Get("id"), r.PostForm["scope"], r.PostForm["settings_version"])
	} else {
		current, err = p.store.PreviewTokenScopesFor(session.userID, r.PostForm.Get("project"), r.PostForm.Get("id"), r.PostForm["scope"], r.PostForm["settings_version"])
	}
	if err != nil {
		p.scopeEditorError(w, r, session, err)
		return
	}
	if intent == "apply" {
		p.redirectNotice(w, r, projectURL("settings", current.Project), "token-scopes-saved")
		return
	}
	step := "review"
	if intent == "back" {
		step = "select"
	}
	editor := newScopeEditor(current, r.PostForm["scope"], step, "", false, false)
	if editor.Noop {
		editor.Step = "select"
		editor.Notice = "변경한 권한이 없습니다."
		editor.Proposed = access.DelegatedScopes(current.Scopes)
	}
	p.renderScopeEditor(w, r, session, 200, editor)
}
