package channel

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func recipientResult(code, message string) *mcp.CallToolResult {
	result := protocol.SendResult{Code: code, Message: message}
	raw, _ := json.Marshal(result)
	return &mcp.CallToolResult{StructuredContent: result, Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}
}
func recipientExpectationAllowed(ctx context.Context, u Upstream) (bool, error) {
	if s, ok := u.(*mcp.ClientSession); ok {
		i := s.InitializeResult()
		return i != nil && i.Capabilities.Experimental[protocol.RecipientExpectationCapability] == true, nil
	}
	if s, ok := u.(interface {
		recipientExpectationSupported(context.Context) (bool, error)
	}); ok {
		return s.recipientExpectationSupported(ctx)
	}
	return false, nil
}
func callWithRecipientExpectation(ctx context.Context, u Upstream, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if path, present := attachmentArgument(p); present {
		if sender, ok := u.(interface {
			sendAttachment(context.Context, *mcp.CallToolParams, string) (*mcp.CallToolResult, error)
		}); ok {
			return sender.sendAttachment(ctx, p, path)
		}
		return recipientResult("unsupported", ""), nil
	}
	if p.Name == "send_message" {
		raw, err := json.Marshal(p.Arguments)
		if err != nil {
			return recipientResult(protocol.CodeInvalid, ""), nil
		}
		expect, err := protocol.RecipientExpectation(raw)
		if err != nil {
			return recipientResult(protocol.CodeInvalid, ""), nil
		}
		if expect != nil {
			supported, err := recipientExpectationAllowed(ctx, u)
			if err != nil {
				return nil, err
			}
			if !supported {
				return recipientResult(protocol.CodeUnsupported, "expectRegistrationId is unsupported by this broker; the expectation was not removed and no message was sent"), nil
			}
		}
	}
	return u.CallTool(ctx, p)
}
func (u *brokerUpstream) recipientExpectationSupported(ctx context.Context) (bool, error) {
	s, err := u.session(ctx)
	if err != nil {
		return false, err
	}
	i := s.InitializeResult()
	return i != nil && i.Capabilities.Experimental[protocol.RecipientExpectationCapability] == true, nil
}
func (u *receiveUpstream) recipientExpectationSupported(ctx context.Context) (bool, error) {
	return recipientExpectationAllowed(ctx, u.Upstream)
}
