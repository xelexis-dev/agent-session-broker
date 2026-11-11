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
			results = append(results, controlpanel.AnnouncementResult{Project: p.Name, Error: "project-deleting"})
			continue
		}
		service, err := b.factory.RemoteBackend(ctx, p.Name)
		if err != nil {
			results = append(results, controlpanel.AnnouncementResult{Project: p.Name, Error: "sessions-unavailable"})
			continue
		}
		sender, ok := service.(interface {
			SendAnnouncement(context.Context, string, string) (broker.SendResult, error)
		})
		sessions, err := service.ListSessions(ctx)
		if err != nil || !ok {
			results = append(results, controlpanel.AnnouncementResult{Project: p.Name, Error: "announcement-unavailable"})
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
			result.Error = "not-ready"
		} else {
			selector := broker.SelectorSession + t.session.ID
			if t.session.RegistrationID != "" {
				selector = broker.SelectorRegistration + t.session.RegistrationID
			}
			sent, err := t.sender.SendAnnouncement(ctx, selector, body)
			switch {
			case errors.Is(err, broker.ErrQueueFull):
				result.Error = "queue-full"
			case errors.Is(err, broker.ErrSessionSuspended):
				result.Error = "suspended"
			case err != nil || !sent.Accepted:
				result.Error = "not-accepted"
			default:
				result.Accepted = true
			}
		}
		results = append(results, result)
	}
	return results, nil
}
