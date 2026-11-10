package controlpanel

import (
	"context"
	"errors"
	"strings"
)

func (p *panel) authorizeMutation(actor, path, project string) error {
	user, ok := p.store.User(actor)
	if !ok || !user.Enabled {
		return ErrForbidden
	}
	if strings.HasPrefix(path, "/admin/users/") && user.Role != "admin" {
		return ErrForbidden
	}
	if path != "/projects/create" && path != "/logout" && !strings.HasPrefix(path, "/admin/users/") {
		role := p.store.ProjectRole(actor, project)
		if role == "" || role == "viewer" {
			return ErrForbidden
		}
		if (path == "/projects/save" || path == "/projects/members") && role != "owner" && user.Role != "admin" {
			return ErrForbidden
		}
	}
	return nil
}

type sessionMutationError struct {
	cause   error
	status  int
	message string
}

func (e *sessionMutationError) Error() string { return e.message }
func (e *sessionMutationError) Unwrap() error { return e.cause }

func (p *panel) manageSession(ctx context.Context, actor, project, op, id, grantID string, guard commitGuard, byRegistration ...bool) (string, error) {
	release, err := p.store.startProjectOperationGuarded(project, nil, guard)
	if err != nil {
		return "", err
	}
	defer release()
	if (op != "/sessions/detach" && op != "/sessions/suspend" && op != "/sessions/unsuspend") || (id == "" && grantID == "") || (id != "" && grantID != "") || len(id) > 256 || len(grantID) > 256 || (grantID != "" && op != "/sessions/detach") {
		return "", &sessionMutationError{nil, 400, "올바른 세션을 선택해 주세요."}
	}
	sessions, err := p.backend.Sessions(ctx, project)
	if err != nil {
		return "", &sessionMutationError{err, 400, "세션 정보를 불러오지 못했습니다."}
	}
	if grantID != "" {
		if err := p.store.releaseReservedGrantFor(actor, project, grantID, sessions); err != nil {
			return "", &sessionMutationError{err, 400, "닉네임 예약을 해제하지 못했습니다. 현재 등록 상태와 권한을 확인해 주세요."}
		}
		return "session-grant-released", nil
	}
	permitted, targetName := false, ""
	for _, item := range sessions {
		if (len(byRegistration) == 0 || !byRegistration[0]) && item.ID == id || len(byRegistration) == 1 && byRegistration[0] && item.RegistrationID == id {
			id = item.ID
			permitted = p.store.CanManageSession(actor, project, sessionIdentity(item), item.OwnerID)
			targetName = item.Name
			break
		}
	}
	if !permitted {
		return "", ErrForbidden
	}
	if op != "/sessions/detach" {
		manager, ok := p.backend.(interface {
			SetSuspended(context.Context, string, string, bool) error
		})
		if !ok || manager.SetSuspended(ctx, project, id, op == "/sessions/suspend") != nil {
			return "", &sessionMutationError{nil, 409, "세션 일시 중지 상태를 변경하지 못했습니다."}
		}
		if op == "/sessions/suspend" {
			p.cancelSuspendedRequests(project, targetName)
			return "session-suspended", nil
		}
		return "session-unsuspended", nil
	}
	if err := p.backend.Detach(ctx, project, id); err != nil {
		return "", &sessionMutationError{err, 409, "세션 연결을 해제하지 못했습니다."}
	}
	return "session-detached", nil
}

var errDeviceCodeInput = errors.New("invalid device code")
var errDeviceCodeAttempts = errors.New("device code attempt limit")

func (p *panel) decideDeviceCode(actor, code, action, nickname string, guard commitGuard) (DeviceView, error) {
	user, ok := p.store.User(actor)
	if !ok || !user.Enabled {
		return DeviceView{}, ErrForbidden
	}
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 32 {
		return DeviceView{}, errDeviceCodeInput
	}
	if !p.allowCodeAttempt(actor) {
		return DeviceView{}, errDeviceCodeAttempts
	}
	switch action {
	case "approve":
		return p.store.approveDeviceAuthorizationGuarded(actor, code, strings.TrimSpace(nickname), false, guard)
	case "deny":
		return p.store.approveDeviceAuthorizationGuarded(actor, code, "", true, guard)
	default:
		return p.store.deviceAuthorizationForGuarded(actor, code, guard)
	}
}
