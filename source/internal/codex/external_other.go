//go:build !windows && !linux && !darwin

package codex

import "context"

func externalProcessKey(context.Context, int) (string, error) { return "", ErrSessionNotFound }
