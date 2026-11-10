package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/attachmentclient"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func attachmentBroker(up Upstream) *brokerUpstream {
	switch u := up.(type) {
	case *brokerUpstream:
		return u
	case *receiveUpstream:
		return attachmentBroker(u.Upstream)
	}
	return nil
}
func (u *brokerUpstream) attachmentSupported(ctx context.Context) bool {
	s, e := u.session(ctx)
	if e != nil {
		return false
	}
	i := s.InitializeResult()
	return i != nil && i.Capabilities.Experimental[broker.AttachmentCapability] == true
}
func (u *brokerUpstream) attachmentRequest(ctx context.Context, method, path, selector, value string, body io.Reader, size int64, name string) (*http.Response, error) {
	endpoint, err := url.Parse(u.endpoint)
	if err != nil {
		return nil, attachmentclient.ErrUnsupported
	}
	endpoint.Path = path
	endpoint.RawPath = ""
	q := endpoint.Query()
	q.Set(selector, value)
	endpoint.RawQuery = q.Encode()
	endpoint.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, attachmentclient.ErrUnsupported
	}
	req.ContentLength = size
	if name != "" {
		req.Header.Set("X-XBroker-Attachment-Name", name)
	}
	u.mu.Lock()
	next := u.transport
	if next == nil {
		next = http.DefaultTransport
	}
	policy := u.policy
	if policy != nil {
		policy = newHTTPPolicyGuard(u.endpoint, &http.Client{Transport: next})
		policy.projectState, policy.projectSource = u.projectState, u.projectSource
	}
	tr := headerTransport{projectState: u.projectState, projectSource: u.projectSource, projectFailure: &u.projectFailure, header: u.header.Clone(), next: next, policy: policy, suspended: &u.suspended, rejected: &u.rejected}
	u.mu.Unlock()
	return (&http.Client{Transport: tr, CheckRedirect: noRedirect}).Do(req)
}
func attachmentHTTPCode(response *http.Response) string {
	var data struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&data)
	switch data.Code {
	case "attachments-disabled", "attachment-unsupported", "attachment-quota", "busy", "server-disk-low", "unavailable", "expired":
		return data.Code
	}
	return "attachment-failed"
}

type attachmentSender struct {
	Upstream
	binding *ingressBinding
	state   string
}

func (u *attachmentSender) sendAttachment(ctx context.Context, p *mcp.CallToolParams, path string) (*mcp.CallToolResult, error) {
	b := u.binding
	b.attachmentMu.Lock()
	defer b.attachmentMu.Unlock()
	up := attachmentBroker(u.Upstream)
	if up == nil || !up.attachmentSupported(ctx) {
		return recipientResult("unsupported", ""), nil
	}
	b.mu.Lock()
	id := b.identity
	active := b.started && !b.failed
	b.mu.Unlock()
	if !active {
		return recipientResult("unsupported", ""), nil
	}
	raw, err := json.Marshal(p.Arguments)
	if err != nil {
		return recipientResult(broker.CodeInvalid, ""), nil
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil {
		return recipientResult(broker.CodeInvalid, ""), nil
	}
	if _, err = broker.RecipientExpectation(raw); err != nil {
		return recipientResult(broker.CodeInvalid, ""), nil
	}
	var from string
	_ = json.Unmarshal(args["from"], &from)
	if from != id.Name {
		return recipientResult(broker.CodeInvalid, ""), nil
	}
	if expected, _ := broker.RecipientExpectation(raw); expected != nil {
		allowed, e := recipientExpectationAllowed(ctx, u.Upstream)
		if e != nil || !allowed {
			return recipientResult("unsupported", ""), nil
		}
	}
	file, err := attachmentclient.OpenSource(id.CWD, path, u.state)
	if err != nil {
		return recipientResult(err.Error(), ""), nil
	}
	defer file.Close()
	transfer, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	hash := sha256.New()
	counter := &attachmentCounter{reader: io.TeeReader(io.LimitReader(file, file.Size+1), hash)}
	response, err := up.attachmentRequest(transfer, http.MethodPost, "/attachments", "from", id.Name, counter, file.Size, file.Name)
	if err != nil {
		return recipientResult("outcome-unknown", "do not automatically resend"), nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return recipientResult(attachmentHTTPCode(response), ""), nil
	}
	var ref broker.Attachment
	if json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&ref) != nil || counter.n != file.Size || ref.Size != file.Size || ref.SHA256 != hex.EncodeToString(hash.Sum(nil)) || !attachmentclient.ValidID(ref.ID) {
		return recipientResult("outcome-unknown", "do not automatically resend"), nil
	}
	delete(args, "attachment")
	copy := *p
	copy.Arguments = args
	copy.Meta = mcp.Meta{}
	for key, value := range p.Meta {
		copy.Meta[key] = value
	}
	copy.Meta[broker.AttachmentMetaKey] = ref.ID
	// The exact CallTool connection rechecks attachment capability in upstream.go.
	result, err := callWithRecipientExpectation(ctx, u.Upstream, &copy)
	if err != nil {
		return recipientResult("outcome-unknown", "do not automatically resend"), nil
	}
	return result, nil
}

type attachmentCounter struct {
	reader io.Reader
	n      int64
}

func (c *attachmentCounter) Read(p []byte) (int, error) {
	n, e := c.reader.Read(p)
	c.n += int64(n)
	return n, e
}

func (srv *ingressServer) routeForwardedCall(ctx context.Context, req *mcp.CallToolRequest) (Upstream, string, error) {
	target := srv.upstream
	if srv.opts.device != nil {
		var denied string
		var err error
		target, denied, err = srv.routeDeviceCall(ctx, req)
		if target == nil || err != nil {
			return target, denied, err
		}
	}
	if req.Params.Name != "send_message" {
		return target, "", nil
	}
	if _, present := attachmentArgument(&mcp.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments}); !present {
		return target, "", nil
	}
	id, err := srv.identify(ctx, req)
	if err != nil {
		return nil, "", err
	}
	srv.mu.Lock()
	b := srv.bindings[id.SessionID]
	valid := b != nil && b.identity.PID == id.PID && b.identity.Harness == id.Harness && sameCWD(b.identity.CWD, id.CWD)
	srv.mu.Unlock()
	if !valid {
		return target, "", nil
	}
	state := ""
	if srv.opts.state != nil {
		state = srv.opts.state.Dir()
	}
	return &attachmentSender{Upstream: target, binding: b, state: state}, "", nil
}

func attachmentArgument(p *mcp.CallToolParams) (string, bool) {
	if p.Name != "send_message" {
		return "", false
	}
	data, err := json.Marshal(p.Arguments)
	if err != nil {
		return "", false
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(data, &args) != nil {
		return "", false
	}
	value, present := args["attachment"]
	if !present {
		return "", false
	}
	var path string
	if json.Unmarshal(value, &path) != nil {
		return "", true
	}
	return path, true
}
func localAttachmentSchema(tool *mcp.Tool) {
	raw, _ := json.Marshal(tool.InputSchema)
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil {
		return
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
		schema["properties"] = props
	}
	props["attachment"] = map[string]any{"type": "string", "description": "Optional: relative path of one file under this session's project folder (≤100 MiB) for the single recipient. Credential files, links and files outside the folder are refused. Needs the administrator switch."}
	tool.InputSchema = schema
}

func localAttachment(a *broker.Attachment) *broker.Attachment { return broker.LocalAttachment(a) }

func (o durableOptions) receiveAttachment(ctx context.Context, upstream Upstream, b *ingressBinding, message broker.Message) (broker.Message, error) {
	if message.Attachment == nil {
		return message, nil
	}
	b.mu.Lock()
	registration := ""
	key := ""
	if b.durable != nil {
		key = b.durable.key
		registration = b.durable.registrationID
	}
	b.mu.Unlock()
	cache, err := os.UserCacheDir()
	if o.attachmentCache != "" {
		cache, err = o.attachmentCache, nil
	}
	up := attachmentBroker(upstream)
	if err != nil || up == nil || registration == "" {
		message.Attachment = &broker.Attachment{Error: "failed"}
		return message, nil
	}
	ref := *message.Attachment
	message.Attachment = attachmentclient.Receive(ctx, cache, key, ref, func(fetchCtx context.Context) (io.ReadCloser, int64, string, string) {
		response, e := up.attachmentRequest(fetchCtx, http.MethodGet, "/attachments/"+ref.ID, "recipient", broker.SelectorRegistration+registration, nil, 0, "")
		if e != nil {
			var network *net.OpError
			var policy *httpPolicyTransientError
			if errors.As(e, &policy) || errors.As(e, &network) || errors.Is(e, io.EOF) || errors.Is(e, io.ErrUnexpectedEOF) || errors.Is(e, context.DeadlineExceeded) {
				return nil, 0, "", "retry"
			}
			return nil, 0, "", "failed"
		}
		if response.StatusCode != http.StatusOK {
			code := attachmentHTTPCode(response)
			if code == "attachment-failed" && (response.StatusCode == 408 || response.StatusCode >= 500 && response.StatusCode != 507) {
				code = "retry"
			}
			response.Body.Close()
			return nil, 0, "", code
		}
		return response.Body, response.ContentLength, response.Header.Get("X-XBroker-Attachment-SHA256"), ""
	})
	if ctx.Err() != nil {
		return message, ctx.Err()
	}
	return broker.LocalMessage(message), nil
}
