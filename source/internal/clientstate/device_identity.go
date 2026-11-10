package clientstate

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
)

var errDeviceIdentity = errors.New("receiver-device-identity-unavailable")

type ReceiverIdentity struct {
	mu           sync.Mutex
	initialized  bool
	cause        string
	secretDigest [sha256.Size]byte
	Scope        string
	secretPath   string
	process      func(context.Context, int) (string, error)
	pid          func() int
}

func NewReceiverIdentity(scope string) *ReceiverIdentity {
	return &ReceiverIdentity{Scope: scope, process: receiverProcessIdentity, pid: os.Getpid}
}
func deviceSecretPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errDeviceIdentity
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "x-broker", "device-secret"), nil
	case "windows":
		root := os.Getenv("LOCALAPPDATA")
		if root == "" || !filepath.IsAbs(root) {
			return "", errDeviceIdentity
		}
		return filepath.Join(root, "x-broker", "device-secret"), nil
	default:
		root := os.Getenv("XDG_STATE_HOME")
		if root == "" {
			root = filepath.Join(home, ".local", "state")
		}
		if !filepath.IsAbs(root) {
			return "", errDeviceIdentity
		}
		return filepath.Join(root, "x-broker", "device-secret"), nil
	}
}
func deviceHMAC(secret []byte, domain, scope, value string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("x-broker/receiver/v1/" + domain + "\x00" + scope + "\x00" + value))
	return hex.EncodeToString(m.Sum(nil))
}
func (r *ReceiverIdentity) material(ctx context.Context) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.secretPath
	var err error
	if p == "" {
		p, err = deviceSecretPath()
	}
	if err != nil {
		if ctx.Err() == nil {
			r.cause = "path-unavailable"
		}
		return nil, errDeviceIdentity
	}
	secret, cause, err := loadDeviceSecretCause(ctx, p, !r.initialized)
	// Retain creation evidence across ordinary rereads until another condition occurs.
	if ctx.Err() == nil && cause != "" && (cause != "ok" || (r.cause != "secret-created" && r.cause != "secret-created-lock-present")) {
		r.cause = cause
	}
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(secret)
	// Re-read permissions/content each time, but never rotate a running receiver's identity.
	if r.initialized && digest != r.secretDigest {
		if ctx.Err() == nil {
			r.cause = "secret-changed"
		}
		return nil, errDeviceIdentity
	}
	r.initialized, r.secretDigest = true, digest
	return secret, nil
}
func (r *ReceiverIdentity) Current(ctx context.Context) *protocol.ReceiverHolder {
	secret, err := r.material(ctx)
	if err != nil {
		return nil
	}
	pid := r.pid()
	key, err := r.process(ctx, pid)
	if err != nil || key == "" {
		if ctx.Err() == nil {
			r.setCause("process-identity-unavailable")
		}
		return nil
	}
	return &protocol.ReceiverHolder{PID: pid, DeviceID: deviceHMAC(secret, "device", r.Scope, ""), Incarnation: deviceHMAC(secret, "process", r.Scope, strconv.Itoa(pid)+"\x00"+key)}
}
func (r *ReceiverIdentity) Dead(ctx context.Context, c protocol.HolderChallenge) bool {
	if !protocol.ValidReceiverHolder(&c.Holder) || ctx.Err() != nil {
		return false
	}
	secret, err := r.material(ctx)
	if err != nil || c.Holder.DeviceID != deviceHMAC(secret, "device", r.Scope, "") {
		return false
	}
	key, err := r.process(ctx, c.Holder.PID)
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil || key == "" {
		r.setCause("process-identity-unavailable")
		return false
	}
	return deviceHMAC(secret, "process", r.Scope, strconv.Itoa(c.Holder.PID)+"\x00"+key) != c.Holder.Incarnation
}

func (r *ReceiverIdentity) DiagnosticCause() string { r.mu.Lock(); defer r.mu.Unlock(); return r.cause }
func (r *ReceiverIdentity) setCause(code string)    { r.mu.Lock(); r.cause = code; r.mu.Unlock() }

// Serialize local first use before opening the OS lock; the OS lock also
// fences independent clients/processes. Cancellation never replaces a secret.
var deviceSecretSlot = make(chan struct{}, 1)

func loadDeviceSecretCause(ctx context.Context, path string, create bool) ([]byte, string, error) {
	select {
	case deviceSecretSlot <- struct{}{}:
		defer func() { <-deviceSecretSlot }()
	case <-ctx.Done():
		return nil, "", errDeviceIdentity
	}
	return loadDeviceSecretPlatform(ctx, path, create)
}
