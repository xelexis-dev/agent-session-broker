package serverruntime

import (
	"context"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Announce uses only projects owned by this instance and the existing queues.
func (b panelBackend) Announce(ctx context.Context, actor, project, body string) ([]controlpanel.AnnouncementResult, error) {
	user, ok := b.store.User(actor)
	if !ok || !user.Enabled || user.Role != "admin" {
		return nil, controlpanel.ErrForbidden
	}
	if err := protocol.ValidateAnnouncement(body, "", ""); err != nil {
		return nil, controlpanel.ErrInvalid
	}
	type target struct {
		project string
		session broker.Session
		sender  interface {
			SendAnnouncement(context.Context, string, string) (broker.SendResult, error)
		}
	}
	var targets []target
	var results []controlpanel.AnnouncementResult
	found := project == ""
	for _, p := range b.store.ProjectsFor(actor) {
		if project != "" && p.Name != project {
			continue
		}
		found = true
		if p.Deleting {
			results = append(results, controlpanel.AnnouncementResult{Project: p.Name, Error: "삭제 중인 프로젝트입니다."})
			continue
		}
		service, err := b.factory.RemoteBackend(ctx, p.Name)
		if err != nil {
			results = append(results, controlpanel.AnnouncementResult{Project: p.Name, Error: "프로젝트 세션을 조회하지 못했습니다."})
			continue
		}
		sender, ok := service.(interface {
			SendAnnouncement(context.Context, string, string) (broker.SendResult, error)
		})
		sessions, err := service.ListSessions(ctx)
		if err != nil || !ok {
			results = append(results, controlpanel.AnnouncementResult{Project: p.Name, Error: "프로젝트 공지를 사용할 수 없습니다."})
			continue
		}
		for _, s := range sessions {
			// Preflight every target before the first enqueue, so an oversized envelope
			// cannot produce a partially sent invalid announcement.
			if err := protocol.ValidateAnnouncement(body, s.Name, s.RegistrationID); err != nil {
				return nil, controlpanel.ErrInvalid
			}
			targets = append(targets, target{p.Name, s, sender})
		}
	}
	if !found {
		return nil, controlpanel.ErrInvalid
	}
	for _, t := range targets {
		result := controlpanel.AnnouncementResult{Project: t.project, SessionID: t.session.ID, Name: t.session.Name}
		if !t.session.AutomaticDeliveryReady {
			result.Error = "자동 수신 준비가 되지 않았습니다."
		} else {
			selector := broker.SelectorSession + t.session.ID
			if t.session.RegistrationID != "" {
				selector = broker.SelectorRegistration + t.session.RegistrationID
			}
			sent, err := t.sender.SendAnnouncement(ctx, selector, body)
			switch {
			case errors.Is(err, broker.ErrQueueFull):
				result.Error = "대기 큐가 가득 찼습니다."
			case errors.Is(err, broker.ErrSessionSuspended):
				result.Error = "일시 중지된 세션입니다."
			case err != nil || !sent.Accepted:
				result.Error = "세션에 접수하지 못했습니다. 연결 상태를 확인하세요."
			default:
				result.Accepted = true
			}
		}
		results = append(results, result)
	}
	return results, nil
}
