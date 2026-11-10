package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const maxEnvelopeBytes = 64 * 1024

// PushExternal queues input to the already running conversation. A successful
// command confirms queue acceptance, not completion of the receiving turn.
func (a *Adapter) PushExternal(ctx context.Context, request broker.AttachRequest, expectedProcessKey string, message broker.Message) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	identity, key, err := a.InspectExternal(ctx, request)
	if err != nil || expectedProcessKey == "" || key != expectedProcessKey {
		if err := ctx.Err(); err != nil {
			return err
		}
		return ErrSessionNotFound
	}
	// macOS prefixes its seconds:microseconds:path key with "darwin:".
	// The full key was compared above; strip only that prefix for extraction.
	// SplitN preserves Windows drive colons and colons inside macOS paths.
	parts := strings.SplitN(strings.TrimPrefix(key, "darwin:"), ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || !filepath.IsAbs(parts[2]) {
		return ErrSessionNotFound
	}
	message = broker.LocalMessage(message)
	payload, err := json.Marshal(broker.DeliveredMessage{Attachment: broker.LocalAttachment(message.Attachment), DeliverySequence: message.DeliverySequence, FromRegistrationID: message.FromRegistrationID, From: message.From, To: message.To, Body: message.Body, SentAt: message.SentAt})
	if err != nil || len(payload) > maxEnvelopeBytes || message.Batch != nil {
		return errors.New("invalid message: maximum envelope size is 64 KiB; batches are receive-only")
	}
	// Windows command-line escaping can double payload bytes. Leave room for
	// the executable, thread UUID, flags, and terminating NUL.
	if runtime.GOOS == "windows" && (len(payload) > 12000 || 2*(len(parts[2])+len(payload))+512 >= 32767) {
		return errors.New("external Codex input exceeds Windows command-line limit (maximum envelope 12000 bytes)")
	}
	run := a.externalQueue
	if run == nil {
		run = runExternalQueue
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err = run(ctx, parts[2], identity.CWD, []string{"queue", "--thread", identity.ID, "--message", string(payload)})
	if err != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		// CLI failures can contain arguments, message bodies, and local details.
		return errors.New("codex automatic input queue failed")
	}
	return nil
}

func runExternalQueue(ctx context.Context, executable, cwd string, args []string) error {
	command := exec.CommandContext(ctx, executable, args...)
	command.Dir = cwd
	// Nil stdin closes input; nil output streams discard CLI diagnostics.
	// Cancellation kills only this queue helper, never the external session PID.
	return command.Run()
}
