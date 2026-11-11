package controlpanel

import (
	"context"
	"errors"
	"net/http"

	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

const projectDataUnavailableMessage = "project data unavailable: the server could not read this project's stored data. Ask the administrator to restore it from a backup."

const projectDataUnavailableNotice = "error.project_data_unavailable"

func projectDataNoticeMessage(err error, fallback string) string {
	if errors.Is(err, sqlitestore.ErrCorrupt) {
		return projectDataUnavailableNotice
	}
	return fallback
}

func projectDataErrorMessage(err error, fallback string) string {
	if errors.Is(err, sqlitestore.ErrCorrupt) {
		return projectDataUnavailableMessage
	}
	return fallback
}

// Audit data lives in the control store and can still be read when the project's
// own store is corrupt. Check the existing read-only policy path, without the
// sessions page's detached-row cleanup, to explain that state on the audit page.
func (p *panel) projectDataNotice(r *http.Request, project string) string {
	policy, ok := p.backend.(interface {
		SessionSuspended(context.Context, string, string) (bool, error)
	})
	if !ok {
		return ""
	}
	release, err := p.store.startProjectOperation(project, nil)
	if err != nil {
		return ""
	}
	defer release()
	_, err = policy.SessionSuspended(r.Context(), project, "")
	return projectDataNoticeMessage(err, "")
}
