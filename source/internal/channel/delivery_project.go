package channel

import (
	"context"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientdelivery"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// Capture provenance before any sink, hook or delayed queue can publish a message.
func boundIngressFactory(project string, factory ingressSinkFactory) ingressSinkFactory {
	return func(id broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		sink, router, err := factory(id)
		if err != nil || sink == nil {
			return sink, router, err
		}
		if router != nil {
			router.mu.Lock()
			if router.project != "" && router.project != project {
				router.mu.Unlock()
				return nil, nil, errors.New("delivery project binding mismatch")
			}
			if router.project != project {
				router.project = project
				push := router.push
				router.push = func(ctx context.Context, m broker.Message) error {
					return push(clientdelivery.WithProject(ctx, project), m)
				}
			}
			router.mu.Unlock()
		}
		return func(ctx context.Context, m broker.Message) error {
			return sink(clientdelivery.WithProject(ctx, project), m)
		}, router, nil
	}
}
