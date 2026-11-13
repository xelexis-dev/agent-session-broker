// Package localactivity observes bounded local harness metadata. It does not
// send samples, control a session, install hooks, or retain message content.
package localactivity

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	Unknown         = "unknown"
	Working         = "working"
	Idle            = "idle"
	ClaudeSessionV1 = "claude-session-v1"
	CodexRolloutV1  = "codex-rollout-v1"
	// CodexTaskPairV1 trusts only explicit same-turn start/complete pairs.
	// Producer versions are evidence, not a compatibility gate.
	CodexTaskPairV1 = "codex-task-pair-v1"
)

// Binding must come from the client's current authenticated lease, not MCP
// arguments. A new lease or process identity requires a new Observer.
type Binding struct {
	Harness, SessionID, ClientID, RegistrationID, LeaseID string
	Generation                                            uint64
}

// Process contains only process-creation metadata. RecordStart is the native
// creation value in the harness file's verified encoding; empty means unknown.
type Process struct{ Key, RecordStart, Host, Platform string }

type Config struct {
	Binding                   Binding
	Home, CWD, Host, Platform string
	PID                       int
	// Format is deliberately empty by default. Callers may select a format
	// only after validating it against their installed harness version.
	RuntimeInfo bool
	Format      string
	Probe       func(context.Context, int) (Process, error)
	Now         func() time.Time
}

// Observation is safe to map to protocol fields. No path, PID, turn ID, raw
// error, token, or content is returned.
type Observation struct {
	Model, Effort         string
	State, Source, Reason string
	Sequence              uint64
	ObservedAt, ChangedAt time.Time
}

type Observer struct {
	mu              sync.Mutex
	config          Config
	sequence        uint64
	processKey      string
	fileInfo        os.FileInfo
	selected        string
	blocked         bool
	rollout         rolloutState
	transcript      transcriptState
	runtimeVerified bool
	located         string
	locateErr       error
	nextLocate      time.Time
	locateBackoff   time.Duration
}

var sessionUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

const staleAfter = 5 * time.Minute

type evidenceError string

func (e evidenceError) Error() string { return string(e) }

const (
	missing        evidenceError = "evidence-missing"
	mismatch       evidenceError = "identity-mismatch"
	malformed      evidenceError = "malformed-evidence"
	budgetExceeded evidenceError = "budget-exceeded"
	unavailable    evidenceError = "observer-unavailable"
	stale          evidenceError = "stale-evidence"
)

// HomeRoot rejects traversal before lexical normalization can hide a symlink parent.
func HomeRoot(root string) string {
	if !filepath.IsAbs(root) || strings.ContainsRune(root, 0) {
		return ""
	}
	for _, component := range strings.FieldsFunc(root, func(r rune) bool { return r == '/' || r == '\\' }) {
		if component == ".." {
			return ""
		}
	}
	return filepath.Clean(root)
}

func New(config Config) *Observer {
	config.Home = HomeRoot(config.Home)
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Host == "" {
		config.Host, _ = os.Hostname()
	}
	if config.Platform == "" {
		config.Platform = runtime.GOOS
	}
	if config.Probe == nil {
		config.Probe = nativeProcess
	}
	return &Observer{config: config}
}

func (o *Observer) Observe(ctx context.Context, current Binding) (out Observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sequence++
	o.runtimeVerified = false
	now := o.config.Now().UTC()
	out = Observation{State: Unknown, Sequence: o.sequence, ObservedAt: now}
	// Transcript metadata does not enable Claude activity or change its reason.
	if o.config.RuntimeInfo && o.config.Binding.Harness == "claude" && o.config.Format == "" {
		defer func() { out.State = Unknown; out.Source = ""; out.Reason = "unsupported" }()
	}
	if !(o.config.RuntimeInfo && o.config.Binding.Harness == "claude" && o.config.Format == "") && (o.config.Format == "" || (o.config.Binding.Harness == "claude" && o.config.Format != ClaudeSessionV1) || (o.config.Binding.Harness == "codex" && o.config.Format != CodexRolloutV1 && o.config.Format != CodexTaskPairV1)) {
		out.Reason = "unsupported"
		return out
	}
	if o.config.Binding.Harness == "claude" {
		out.Source = "claude-session-file"
	} else if o.config.Binding.Harness == "codex" {
		out.Source = "codex-rollout"
	} else {
		out.Reason = "unsupported"
		return out
	}
	if o.blocked || current != o.config.Binding || !validBinding(current) {
		o.blocked = true
		out.Reason = string(mismatch)
		return out
	}
	process, err := o.config.Probe(ctx, o.config.PID)
	if err != nil || ctx.Err() != nil {
		out.Reason = string(unavailable)
		return out
	}
	if process.Key == "" || process.Host == "" || !strings.EqualFold(process.Host, o.config.Host) || process.Platform != o.config.Platform || !filepath.IsAbs(o.config.CWD) || !filepath.IsAbs(o.config.Home) || o.config.PID <= 0 || o.config.PID > 1<<31-1 {
		out.Reason = string(mismatch)
		return out
	}
	if o.processKey != "" && o.processKey != process.Key {
		o.blocked = true
		out.Reason = string(mismatch)
		return out
	}
	o.processKey = process.Key
	limit := int64(1 << 20)
	if o.sequence == 1 {
		limit = 8 << 20
	}
	if current.Harness == "claude" && o.config.Format != "" {
		limit = 64 << 10
	}
	b := &readBudget{ctx: ctx, left: limit}
	var state string
	var changed time.Time
	if current.Harness == "claude" {
		if o.config.RuntimeInfo && o.config.Format == "" {
			err = o.observeTranscript(b)
			state = Unknown
			out.Reason = "unsupported"
		} else {
			state, changed, err = o.observeClaude(b, process, now)
		}
	} else {
		state, changed, err = o.observeCodex(b, now)
	}
	after, probeErr := o.config.Probe(ctx, o.config.PID)
	if probeErr != nil || ctx.Err() != nil {
		err = unavailable
	} else if after != process {
		err = mismatch
		o.blocked = true
	}
	if o.runtimeVerified && (err == nil || err == stale || err == missing || err == o.rollout.reason && err == malformed) {
		if current.Harness == "codex" {
			out.Model, out.Effort = o.rollout.model, o.rollout.effort
		} else if o.config.RuntimeInfo {
			out.Model, out.Effort = o.transcript.model, o.transcript.effort
		}
	}
	if err != nil {
		out.Reason = string(unavailable)
		if reason, ok := err.(evidenceError); ok {
			out.Reason = string(reason)
		}
		// Only a committed parse may retain its pair reason or missing tail.
		if current.Harness == "codex" && !(o.runtimeVerified && (err == stale || err == missing || err == o.rollout.reason)) {
			o.rollout = rolloutState{}
		}
		return out
	}
	out.State, out.ChangedAt = state, changed
	return out
}

func validBinding(b Binding) bool {
	if !sessionUUID.MatchString(b.SessionID) || b.Generation == 0 {
		return false
	}
	for _, s := range []string{b.ClientID, b.RegistrationID, b.LeaseID} {
		if s == "" || len(s) > 256 || strings.ContainsAny(s, "\x00\r\n") {
			return false
		}
	}
	return true
}

func trustedTime(at, now time.Time) bool { return !at.IsZero() && !at.After(now.Add(time.Minute)) }
func samePath(a, b string) bool {
	if !filepath.IsAbs(a) || !filepath.IsAbs(b) {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
func turnKey(s string) ([32]byte, bool) {
	if len(s) == 0 || len(s) > 128 {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(s)), true
}
