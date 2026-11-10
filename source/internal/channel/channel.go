// Package channel bridges a local broker to Claude's official stdio channel.
package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type Upstream interface {
	CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error)
}
type capturedTransport struct {
	mcp.Transport
	conn mcp.Connection
}

func (t *capturedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	c, e := t.Transport.Connect(ctx)
	t.conn = c
	return c, e
}
func (t *capturedTransport) notify(ctx context.Context, content string, meta map[string]string) error {
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/claude/channel", "params": map[string]any{"content": content, "meta": meta}})
	if err != nil {
		return err
	}
	message, err := jsonrpc.DecodeMessage(data)
	if err != nil {
		return err
	}
	return t.conn.Write(ctx, message)
}
func call(ctx context.Context, upstream Upstream, name string, args any, out any) error {
	result, err := callWithRecipientExpectation(ctx, upstream, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		var project *projectRefusalError
		if errors.As(err, &project) {
			return err
		}
		if upstreamRejected(upstream) {
			return errors.New(upstreamCredentialGuidance(upstream, false))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("broker %s transport failed", name)
	}
	if result.IsError {
		if state := upstreamCompatibility(upstream); !state.Compatible {
			return &compatibilityError{state.Message}
		}
		if insufficientScope(result) {
			return errInsufficientScope
		}
		if name == "rename_session" {
			return safeRenameRejection(result)
		}
		if name == "attach_session" {
			// Read only the reason code; never copy server text or messages.
			var reason struct {
				Code string `json:"code"`
			}
			data, marshalErr := json.Marshal(result.StructuredContent)
			if marshalErr == nil {
				_ = json.Unmarshal(data, &reason)
			}
			return broker.SafeAttachRejection(reason.Code)
		}
		if name == "wait_for_message" {
			for _, content := range result.Content {
				if text, ok := content.(*mcp.TextContent); ok {
					if reason := waitRefusalReason(text.Text); reason != "" {
						return &waitRefusalError{reason}
					}
				}
			}
		}
		return fmt.Errorf("broker %s rejected the request", name)
	}
	if out == nil {
		return nil
	}
	var data []byte
	if result.StructuredContent != nil {
		data, err = json.Marshal(result.StructuredContent)
	} else if len(result.Content) > 0 {
		if t, ok := result.Content[0].(*mcp.TextContent); ok {
			data = []byte(t.Text)
		}
	}
	if err != nil || len(data) == 0 {
		return errors.New("broker returned no structured result")
	}
	return json.Unmarshal(data, out)
}

func maintainRegistration(ctx context.Context, upstream Upstream, identity broker.AttachRequest, interval, timeout time.Duration) error {
	identity.Capabilities = []string{broker.MessageSentAtCapability}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			renewCtx, stop := context.WithTimeout(ctx, timeout)
			err := call(renewCtx, upstream, "attach_session", identity, nil)
			stop()
			// Slow local inspection must not permanently disable a live channel.
			// The broker independently expires readiness if renewals stop arriving.
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				continue
			}
			if err != nil {
				return err
			}
		}
	}
}

func messageMetadata(message broker.Message) map[string]string {
	meta := map[string]string{"from": message.From, "to": message.To}
	if message.DeliverySequence != 0 {
		meta["deliverySequence"] = strconv.FormatUint(message.DeliverySequence, 10)
	}
	if message.FromRegistrationID != "" {
		meta["fromRegistrationId"] = message.FromRegistrationID
	}
	if !message.SentAt.IsZero() {
		meta["sentAt"] = message.SentAt.UTC().Format(time.RFC3339Nano)
	}
	if a := broker.LocalAttachment(message.Attachment); a != nil {
		if a.Error != "" {
			meta["attachment_error"] = a.Error
		} else {
			meta["attachment_path"] = a.Path
			meta["attachment_size"] = strconv.FormatInt(a.Size, 10)
			meta["attachment_sha256"] = a.SHA256
		}
	}
	return meta
}
