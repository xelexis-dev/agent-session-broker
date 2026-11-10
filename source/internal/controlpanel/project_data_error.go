package controlpanel

import (
	"context"
	"errors"
	"net/http"

	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

const projectDataUnavailableMessage = "project data unavailable: the server could not read this project's stored data. Ask the administrator to restore it from a backup."

const projectDataUnavailableNotice = "이 프로젝트의 저장 데이터를 읽을 수 없습니다. 관리자가 백업에서 복원해야 합니다."

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
