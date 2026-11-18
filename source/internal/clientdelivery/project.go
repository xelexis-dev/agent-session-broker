// Package clientdelivery carries client-owned display provenance, never wire authority.
package clientdelivery

import (
	"context"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type projectKey struct{}

func WithProject(ctx context.Context, project string) context.Context {
	return context.WithValue(ctx, projectKey{}, project)
}
func Project(ctx context.Context) string {
	project, _ := ctx.Value(projectKey{}).(string)
	return project
}

type Message struct {
	Project string `json:"project,omitempty"`
	broker.Message
}

type Entry struct {
	Project string `json:"project,omitempty"`
	broker.DeliveredMessage
}
