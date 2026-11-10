// Package nativeinbox posts to the current Claude MCP parent's inherited local
// inbox. Credentials stay in this process; they are never discovered from disk.
package nativeinbox

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const (
	maxFrameBytes    = 64 * 1024
	maxBatchMessages = 32
	postTimeout      = 5 * time.Second
)

var (
	ErrUnavailable    = errors.New("native inbox unavailable for this session")
	ErrInvalidMessage = errors.New("native inbox message exceeds limits or has invalid structure")
	ErrDelivery       = errors.New("native inbox write failed")
)

type writeConnection interface {
	io.WriteCloser
	SetWriteDeadline(time.Time) error
}

// Poster holds only the endpoint and credentials inherited by this MCP child.
// Each Post opens its own bounded connection; callers may post concurrently.
type Poster struct {
	peerTag      bool
	parentPID    int
	verifyParent func(context.Context) bool
	endpoint     string
	token        string
	timeout      time.Duration
	dial         func(context.Context, string) (writeConnection, error)
}

// FromEnvironment requires the observed parent identity to match the inherited
// session UUID exactly. No fallback selects another session or reads token files.
func FromEnvironment(expectedSessionID string) (*Poster, error) {
	if !canonicalUUID(expectedSessionID) || os.Getenv("CLAUDE_CODE_SESSION_ID") != expectedSessionID {
		return nil, fmt.Errorf("%w: inherited session identity mismatch", ErrUnavailable)
	}
	token := os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN")
	if token == "" || len(token) > 4096 {
		return nil, fmt.Errorf("%w: inherited credential missing or invalid", ErrUnavailable)
	}
	endpoint, err := socketPath(os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"))
	if err != nil {
		return nil, fmt.Errorf("%w: inherited endpoint format unsupported", ErrUnavailable)
	}
	return &Poster{endpoint: endpoint, token: token, timeout: postTimeout, dial: dialLocal, peerTag: os.Getenv("CLAUDE_CODE_ENTRYPOINT") == "claude-vscode"}, nil
}

// FromParentEnvironment binds the inherited inbox to its actual server process.
// verifyCurrent rechecks the parent after dialing, before any credential write.
func FromParentEnvironment(parentPID int, verifyCurrent func(context.Context) bool) (*Poster, error) {
	if parentPID <= 0 || verifyCurrent == nil {
		return nil, ErrUnavailable
	}
	p, err := FromEnvironment(os.Getenv("CLAUDE_CODE_SESSION_ID"))
	if err != nil {
		return nil, err
	}
	p.parentPID, p.verifyParent = parentPID, verifyCurrent
	return p, nil
}

func canonicalUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Post authenticates, then writes one user frame per message in FIFO order.
// Success means local writes completed, not authentication acknowledgement or
// model receipt. Errors can follow partial writes; callers must not blindly retry.
func (p *Poster) Post(ctx context.Context, message broker.Message) error {
	if p == nil || p.dial == nil {
		return ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	frames, err := encodeFrames(message, p.peerTag)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	conn, err := p.dial(ctx, p.endpoint)
	if err != nil {
		return deliveryError(ctx)
	}
	defer conn.Close()
	if p.parentPID != 0 && (!connectionFromParent(conn, p.parentPID) || !p.verifyParent(ctx)) {
		return ErrUnavailable
	}
	deadline, _ := ctx.Deadline()
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return deliveryError(ctx)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetWriteDeadline(time.Now()) })
	defer stop()
	auth, _ := json.Marshal(struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}{Type: "auth", Token: p.token})
	if err := writeFrame(conn, auth); err != nil {
		return deliveryError(ctx)
	}
	for _, frame := range frames {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeFrame(conn, frame); err != nil {
			return deliveryError(ctx)
		}
	}
	return nil
}

func deliveryError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Transport errors may include endpoints or credentials. Never wrap them.
	return ErrDelivery
}

func writeFrame(writer io.Writer, frame []byte) error {
	data := append(frame, '\n')
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

type panelEnvelope struct {
	From               string             `json:"from"`
	Body               string             `json:"body"`
	To                 string             `json:"to"`
	DeliverySequence   uint64             `json:"deliverySequence,omitempty"`
	FromRegistrationID string             `json:"fromRegistrationId,omitempty"`
	SentAt             time.Time          `json:"sentAt,omitzero"`
	Attachment         *broker.Attachment `json:"attachment,omitempty"`
}

func encodeFrames(message broker.Message, peerTag bool) ([][]byte, error) {
	message = broker.LocalMessage(message)
	messages := []broker.DeliveredMessage{{Attachment: broker.LocalAttachment(message.Attachment), DeliverySequence: message.DeliverySequence, FromRegistrationID: message.FromRegistrationID, From: message.From, To: message.To, Body: message.Body, SentAt: message.SentAt}}
	if message.Batch != nil {
		if message.From != "" || message.To != "" || message.Body != "" || len(message.Batch.Messages) == 0 || len(message.Batch.Messages) > maxBatchMessages {
			return nil, ErrInvalidMessage
		}
		messages = message.Batch.Messages
	}
	frames := make([][]byte, 0, len(messages))
	for _, entry := range messages {
		entry.Attachment = broker.LocalAttachment(entry.Attachment)
		// Check individual raw lengths before marshaling to bound allocations.
		if len(entry.FromRegistrationID) > 26 || len(entry.From) > maxFrameBytes || len(entry.To) > maxFrameBytes || len(entry.Body) > maxFrameBytes {
			return nil, ErrInvalidMessage
		}
		content, err := json.Marshal(entry)
		if err != nil || len(content) > maxFrameBytes {
			return nil, ErrInvalidMessage
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return nil, ErrDelivery
		}
		id[6] = (id[6] & 0x0f) | 0x40
		id[8] = (id[8] & 0x3f) | 0x80
		value := struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			From     string `json:"from"`
			UUID     string `json:"uuid"`
			Priority string `json:"priority"`
		}{Type: "user", Message: struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{Role: "user", Content: string(content)}, From: "x-broker", UUID: fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), Priority: "next"}
		frame, err := json.Marshal(value)
		if err != nil || len(frame)+1 > maxFrameBytes {
			return nil, ErrInvalidMessage
		}
		if peerTag {
			// The panel's collapsed preview shows only the start of the envelope.
			visible, err := json.Marshal(panelEnvelope{From: entry.From, Body: entry.Body, To: entry.To, DeliverySequence: entry.DeliverySequence, FromRegistrationID: entry.FromRegistrationID, SentAt: entry.SentAt, Attachment: entry.Attachment})
			if err != nil {
				return nil, ErrInvalidMessage
			}
			// VS Code renders synthetic peer input inside Claude's cross-session tag.
			value.Message.Content = "<cross-session-message>\n" + string(visible) + "\n</cross-session-message>"
			tagged, err := json.Marshal(value)
			if err != nil {
				return nil, ErrInvalidMessage
			}
			if len(tagged)+1 <= maxFrameBytes {
				frame = tagged
			}
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

func stripSocketScheme(value string) (string, error) {
	// Claude inherits a raw endpoint; explicit peer addresses may carry uds:.
	// The platform parser still enforces a local, absolute endpoint below.
	if value == "" || len(value) > 4096 || strings.ContainsRune(value, 0) {
		return "", ErrUnavailable
	}
	return strings.TrimPrefix(value, "uds:"), nil
}
