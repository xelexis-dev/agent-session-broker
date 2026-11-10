//go:build darwin || linux

package channel

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/codexcontrol"
)

type verifiedCodexControl struct{ *codexcontrol.Client }

func (c verifiedCodexControl) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := c.Client.Call(ctx, method, params)
	var rpc *codexcontrol.RPCError
	if errors.As(err, &rpc) {
		err = &steerRPCError{code: rpc.Code, message: rpc.Message}
	}
	return raw, err
}

const codexControlAvailable = true

func bindCodexControl(pid int) func(context.Context) (codexControl, error) {
	host, err := codexcontrol.Bind(pid, kimiProcessKey)
	return func(ctx context.Context) (codexControl, error) {
		if err != nil {
			return nil, err
		}
		c, e := host.Dial(ctx)
		if e != nil {
			return nil, e
		}
		return verifiedCodexControl{c}, nil
	}
}
