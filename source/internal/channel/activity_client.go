package channel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/localactivity"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

const activityBudget = 100 * time.Millisecond

type activityObserver interface {
	Observe(context.Context, localactivity.Binding) localactivity.Observation
}
type activityUpstream interface {
	verifyActivity(context.Context) bool
	activityAllowed() bool
}

// Capability belongs to the actual upstream connection, never the generated
// tool snapshot used to expose tools before a conversation is authorized.
func negotiateActivity(ctx context.Context, u Upstream, version int) bool {
	if version != broker.ActivityVersion {
		return false
	}
	capable, ok := u.(activityUpstream)
	return ok && capable.verifyActivity(ctx)
}
func activitySchemaAllowed(tools []*mcp.Tool) bool {
	return optionalRuntimeSchemaAllowed(tools, "activity")
}
func optionalRuntimeSchemaAllowed(tools []*mcp.Tool, field string) bool {
	for _, tool := range tools {
		if tool == nil || tool.Name != "renew_lease" {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil || len(raw) > 64<<10 {
			return false
		}
		var schema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if json.Unmarshal(raw, &schema) != nil || schema.Type != "object" {
			return false
		}
		var root map[string]json.RawMessage
		if json.Unmarshal(raw, &root) != nil {
			return false
		}
		for _, key := range []string{"$ref", "allOf", "anyOf", "oneOf", "not", "if", "then", "else", "dependentSchemas"} {
			if _, exists := root[key]; exists {
				return false
			}
		}
		for _, key := range schema.Required {
			if key == field {
				return false
			}
		}
		prop, exists := schema.Properties[field]
		if !exists {
			return false
		}
		// Raw optional JSON has an unconstrained schema. An object schema is also
		// accepted; incompatible scalar/false schemas fail closed.
		var value map[string]any
		if json.Unmarshal(prop, &value) != nil || value == nil {
			return false
		}
		typ, hasType := value["type"]
		if hasType && typ != "object" {
			return false
		}
		for k := range value {
			if k != "type" && k != "description" && k != "title" {
				return false
			}
		}
		return true
	}
	return false
}

type activityJob struct {
	ctx     context.Context
	observe activityObserver
	binding localactivity.Binding
	result  chan localactivity.Observation
	stream  *activityStream
}

// A server owns exactly two shared observation workers. There is no per-job
// goroutine or queue. A stuck syscall can occupy a slot but cannot block renew
// or create unbounded workers. Shutdown cancels jobs and joins their owners.
type activityWorkers struct {
	ctx  context.Context
	jobs chan activityJob
	wg   sync.WaitGroup
}

func newActivityWorkers(ctx context.Context) *activityWorkers {
	p := &activityWorkers{ctx: ctx, jobs: make(chan activityJob)}
	for range 2 {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-p.jobs:
					budget, cancel := context.WithTimeout(job.ctx, activityBudget)
					out := job.observe.Observe(budget, job.binding)
					if budget.Err() != nil {
						out = localactivity.Observation{State: localactivity.Unknown, Reason: "budget-exceeded", ObservedAt: time.Now().UTC()}
					}
					cancel()
					select {
					case job.result <- out:
						if job.stream != nil {
							job.stream.observationReady(out)
						}
					default:
					}
				}
			}
		}()
	}
	return p
}

type activityStream struct {
	binding         localactivity.Binding
	observer        activityObserver
	pool            *activityWorkers
	pending         chan localactivity.Observation
	completed       *localactivity.Observation
	wake            chan struct{}
	mu              sync.Mutex // guards the state/wake shared with observation workers
	lastSentState   broker.ActivityState
	transitionWoke  bool
	pendingAt       time.Time
	sequence        uint64
	maxAge          time.Duration
	lastReason      string
	runtimeInfo     json.RawMessage
	diagnosedReason string
}

func (o durableOptions) newActivityStream(b *ingressBinding) *activityStream {
	b.mu.Lock()
	id, d, generation := b.identity, *b.durable, b.generation
	runtimeEnabled := b.runtimeInfoEnabled
	b.mu.Unlock()
	bound := localactivity.Binding{Harness: id.Harness, SessionID: id.SessionID, ClientID: d.clientID, RegistrationID: d.registrationID, LeaseID: id.ChannelID, Generation: generation}
	home, _ := os.UserHomeDir()
	if id.Harness == "claude" {
		home = filepath.Join(home, ".claude")
	} else {
		home = filepath.Join(home, ".codex")
	}
	config := localactivity.Config{Binding: bound, PID: id.PID, CWD: id.CWD, Home: home, RuntimeInfo: runtimeEnabled}
	// Display-only, schema-based Codex support. Claude lacks proven host binding
	// and stays unsupported. No MCP argument/environment override selects formats.
	if id.Harness == "codex" {
		config.Format = localactivity.CodexTaskPairV1
	}
	var observer activityObserver = localactivity.New(config)
	if o.activityFactory != nil {
		observer = o.activityFactory(config)
	}
	return &activityStream{wake: make(chan struct{}, 1), binding: bound, observer: observer, pool: o.activityPool, maxAge: o.heartbeat + 5*time.Second}
}

func (s *activityStream) nextTick() { s.mu.Lock(); s.transitionWoke = false; s.mu.Unlock() }

func observedActivityState(out localactivity.Observation) broker.ActivityState {
	state := broker.ActivityState(out.State)
	if out.Reason != "" || (state != broker.ActivityWorking && state != broker.ActivityIdle) {
		return broker.ActivityUnknown
	}
	return state
}

func (s *activityStream) observationReady(out localactivity.Observation) {
	state := observedActivityState(out)
	s.mu.Lock()
	defer s.mu.Unlock()
	if state == s.lastSentState || s.transitionWoke {
		return
	}
	s.transitionWoke = true
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// A queued notification may have been overtaken by an ACK-driven sample.
func (s *activityStream) transitionReady() bool {
	select {
	case got := <-s.pending:
		s.pending = nil
		s.completed = &got
		s.mu.Lock()
		changed := observedActivityState(got) != s.lastSentState
		s.mu.Unlock()
		return changed
	default:
		return false
	}
}

func (s *activityStream) sample(ctx context.Context, b *ingressBinding, u Upstream) json.RawMessage {
	b.mu.Lock()
	id, d := b.identity, b.durable
	current := localactivity.Binding{Harness: id.Harness, SessionID: id.SessionID, ClientID: d.clientID, RegistrationID: d.registrationID, LeaseID: id.ChannelID, Generation: b.generation}
	enabled, runtimeEnabled := b.activityEnabled, b.runtimeInfoEnabled
	b.mu.Unlock()
	capable, ok := u.(activityUpstream)
	runtimeCapable, runtimeOK := u.(runtimeInfoUpstream)
	runtimeEnabled = runtimeEnabled && runtimeOK && runtimeCapable.runtimeInfoAllowed()
	s.runtimeInfo = nil
	if !(enabled && ok && capable.activityAllowed()) && !runtimeEnabled || current != s.binding || ctx.Err() != nil {
		return nil
	}
	s.sequence++
	out := localactivity.Observation{State: localactivity.Unknown, Reason: "observer-unavailable", ObservedAt: time.Now().UTC()}
	completed := false
	if s.binding.Harness == "kimi" {
		out.Reason = "unsupported"
		completed = true
	}
	if s.completed != nil {
		got := *s.completed
		if !got.ObservedAt.IsZero() && time.Since(got.ObservedAt) <= s.maxAge {
			out = got
			completed = true
		}
		s.completed = nil
	}
	if s.pending != nil {
		select {
		case got := <-s.pending:
			if !got.ObservedAt.IsZero() && time.Since(got.ObservedAt) <= s.maxAge {
				out = got
				completed = true
			}
			s.pending = nil
		default:
			if time.Since(s.pendingAt) >= activityBudget {
				out.Reason = "budget-exceeded"
			}
		}
	}
	activity := broker.Activity{State: broker.ActivityState(out.State), Source: out.Source, Reason: out.Reason, Sequence: s.sequence, Generation: s.binding.Generation, ObservedAt: out.ObservedAt.UTC(), ChangedAt: out.ChangedAt.UTC()}
	// Defensive DTO normalization also protects future adapter implementations.
	switch activity.State {
	case broker.ActivityWorking, broker.ActivityIdle, broker.ActivityUnknown:
	default:
		activity.State = broker.ActivityUnknown
		activity.Reason = "observer-unavailable"
	}
	if activity.Source != "claude-session-file" && activity.Source != "codex-rollout" {
		activity.Source = ""
	}
	switch activity.Reason {
	case "", "unsupported", "evidence-missing", "identity-mismatch", "malformed-evidence", "stale-evidence", "budget-exceeded", "observer-unavailable":
	default:
		activity.State = broker.ActivityUnknown
		activity.Reason = "observer-unavailable"
	}
	if activity.Reason != "" {
		activity.State = broker.ActivityUnknown
	}
	if activity.State != broker.ActivityUnknown && (activity.Source == "" || (s.binding.Harness == "claude" && activity.Source != "claude-session-file") || (s.binding.Harness == "codex" && activity.Source != "codex-rollout")) {
		activity.State = broker.ActivityUnknown
		activity.Source = ""
		activity.Reason = "observer-unavailable"
	}
	if activity.ObservedAt.IsZero() || activity.ObservedAt.After(time.Now().Add(time.Minute)) || activity.ChangedAt.After(activity.ObservedAt) {
		activity.State = broker.ActivityUnknown
		activity.Reason = "observer-unavailable"
		activity.ObservedAt = time.Now().UTC()
		activity.ChangedAt = time.Time{}
	}
	if runtimeEnabled {
		model, effort := out.Model, out.Effort
		if !broker.ValidRuntimeLabel(model, 96) {
			model = ""
		}
		if !broker.ValidRuntimeLabel(effort, 16) {
			effort = ""
		}
		s.runtimeInfo, _ = json.Marshal(broker.RuntimeInfo{Model: model, Effort: effort, Sequence: s.sequence, Generation: s.binding.Generation, ObservedAt: activity.ObservedAt})
	}
	s.mu.Lock()
	s.lastSentState = activity.State
	s.mu.Unlock()
	if s.binding.Harness != "kimi" && s.pending == nil && s.pool != nil && s.observer != nil {
		result := make(chan localactivity.Observation, 1)
		select {
		case <-s.pool.ctx.Done():
		case s.pool.jobs <- activityJob{ctx: ctx, observe: s.observer, binding: s.binding, result: result, stream: s}:
			s.pending = result
			s.pendingAt = time.Now()
		default:
			if !completed {
				activity.Reason = "budget-exceeded"
			}
		}
	}
	raw, _ := json.Marshal(activity)
	if !enabled || !ok || !capable.activityAllowed() {
		raw = nil
	}
	s.lastReason = activity.Reason
	return raw
}

func (s *activityStream) diagnose(opts durableOptions, b *ingressBinding) {
	if s.lastReason != s.diagnosedReason {
		s.diagnosedReason = s.lastReason
		if s.lastReason != "" {
			opts.diagnose(b, "activity-unavailable", s.lastReason, "")
		}
	}
}
