package mcpconfig

import (
	"context"
	"strings"
)

// RecommendedClientVersion is the published npm version required by release preparation.
const RecommendedClientVersion = "1.0.1"

type versionKey struct{}

// Version normalizes the linker-supplied product version for MCP metadata.
func Version(values ...string) string {
	if len(values) > 0 && strings.TrimSpace(values[0]) != "" {
		return strings.TrimSpace(values[0])
	}
	return "dev"
}

// WithVersion carries immutable process metadata through local client startup.
func WithVersion(ctx context.Context, version string) context.Context {
	return context.WithValue(ctx, versionKey{}, Version(version))
}

func ContextVersion(ctx context.Context) string {
	version, _ := ctx.Value(versionKey{}).(string)
	return Version(version)
}
