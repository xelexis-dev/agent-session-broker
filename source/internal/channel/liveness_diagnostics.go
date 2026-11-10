package channel

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

type waitRefusalError struct{ reason string }

func (e *waitRefusalError) Error() string {
	return "broker wait_for_message rejected the request: " + e.reason
}
func waitRefusalReason(message string) string {
	switch message {
	case "remote ingress lease is no longer active":
		return "lease-replaced"
	case "remote delivery channel is not activated":
		return "not-activated"
	case "message acknowledgement requires the current registration and epoch", "invalid message acknowledgement or cursor", "registration mismatch":
		return "ack-invalid"
	}
	return ""
}
func diagnosticErrorClass(err error, rejected bool) string {
	var refusal *waitRefusalError
	if errors.As(err, &refusal) {
		return refusal.reason
	}

	if terminalProjectError(err) {
		return "project-deleted"
	}
	var project *projectRefusalError
	if errors.As(err, &project) {
		switch project.code {
		case "project_disabled":
			return "project-disabled"
		case "project_deleting":
			return "project-deleting"
		}
	}
	if rejected {
		return "authorization"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var network net.Error
	if errors.As(err, &network) {
		return "network"
	}
	var rpc *jsonrpc.Error
	if errors.As(err, &rpc) {
		return "protocol"
	}
	return "other"
}
func diagnosticBrokerReason(code string) string {
	switch code {
	case "invalid", "not-found", "detached", "suspended", "owned", "revision-mismatch", "stale-probe", "storage", "invalid-token", "capacity":
		return code
	default:
		return "other"
	}
}
func receiverReason(exit receiverExit) string {
	switch exit {
	case exitContext:
		return "context"
	case exitUpstream:
		return "upstream"
	case exitSink:
		return "sink"
	default:
		return "terminal"
	}
}
func (o durableOptions) diagnose(b *ingressBinding, event, reason, class string) {
	if o.state == nil || b.durable == nil {
		return
	}
	b.mu.Lock()
	key := b.durable.key
	e := clientstate.DiagnosticEvent{At: time.Now().UTC(), SessionID: b.identity.SessionID, RegistrationID: b.durable.registrationID, Generation: b.generation, Event: event, Reason: reason, ErrorClass: class}
	a := b.durable.session
	b.mu.Unlock()
	if a != nil {
		a.mu.Lock()
		a.diagnosticRegistrationID = e.RegistrationID
		a.diagnosticGeneration = e.Generation
		e.AuthState = a.status
		a.mu.Unlock()
	}
	_ = o.state.RecordDiagnostic(key, e)
}
func (o durableOptions) diagnostics(b *ingressBinding) *clientstate.DiagnosticSummary {
	o.diagnoseDeviceIdentity(b)
	if o.state == nil || b.durable == nil {
		return nil
	}
	b.mu.Lock()
	key := b.durable.key
	b.mu.Unlock()
	out, err := o.state.LoadDiagnostics(key)
	out = clientstate.SafeDiagnostics(out)
	if err != nil {
		out.Summary.Unavailable = true
	}
	return &out.Summary
}

const detachedGuidance = "This session, another session with session:manage, or an administrator detached this registration. Register again with attach_session only when the user asks to connect."

func detachedStatusMessage(message string) string {
	if !strings.Contains(message, "registration-detached") {
		message = "registration-detached: " + message
	}
	if !strings.Contains(message, detachedGuidance) {
		message += " " + detachedGuidance
	}
	return message
}

func (o durableOptions) detachedObserved(b *ingressBinding) {
	b.mu.Lock()
	b.resumeStopReason = "registration-detached"
	d := b.durable
	if d == nil {
		b.mu.Unlock()
		return
	}
	d.detached = true
	a := d.session
	id, saved := b.identity, *d
	b.mu.Unlock()
	if a != nil {
		a.mu.Lock()
		a.state.Registration.Detached = true
		a.mu.Unlock()
		_ = o.persistSession(a)
	} else if o.state != nil {
		_ = o.save(id, &saved)
	}
}

func (o durableOptions) diagnoseRenew(b *ingressBinding, upstream Upstream, result broker.RegisterResult, err error) {
	b.mu.Lock()
	if err == nil {
		b.resumeSuspended = result.Code == broker.CodeSuspended
	} else if upstreamSuspended(upstream) {
		b.resumeSuspended = true
	}
	b.mu.Unlock()
	if err != nil {
		class := diagnosticErrorClass(err, upstreamRejected(upstream))
		o.diagnose(b, "renew-failed", class, class)
	} else if result.Code != "" {
		if result.Code == broker.CodeDetached {
			o.detachedObserved(b)
		}
		o.diagnose(b, "renew-failed", diagnosticBrokerReason(result.Code), "protocol")
	} else {
		o.diagnose(b, "renew-success", "", "")
	}
}
