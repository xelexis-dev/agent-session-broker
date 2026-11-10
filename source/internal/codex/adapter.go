package codex

import (
	"context"
	"errors"
)

var ErrSessionNotFound = errors.New("codex session not found")

type Adapter struct {
	externalHome  string
	externalProbe func(context.Context, int) (string, error)
	externalQueue func(context.Context, string, string, []string) error
}

func NewAdapter() *Adapter { return &Adapter{} }
