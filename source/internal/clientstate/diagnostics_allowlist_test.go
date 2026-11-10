package clientstate

import (
	"slices"
	"testing"
	"time"
)

// Accept sets frozen from the literal lists before they were collapsed into
// named slices. A persisted value must stay accepted (and an unknown one
// rejected) exactly as before, or old readers and rollback break.
var frozenDiagnosticAccept = map[string][]string{
	"knownReasonsWithoutDevice": {"", "ack-invalid", "approval-required", "authorization", "budget-exceeded", "cancelled", "capacity", "client-mismatch", "context", "deadline", "detached", "evidence-missing", "identity-conflict", "identity-mismatch", "invalid", "invalid-token", "lease-replaced", "malformed-evidence", "network", "not-activated", "not-found", "observer-unavailable", "other", "owned", "owned-local", "owner-mismatch", "project-deleted", "project-deleting", "project-disabled", "protocol", "refused", "registration-in-use", "revision-mismatch", "secret-mismatch", "sink", "stale-evidence", "stale-probe", "still-receiving", "storage", "suspended", "terminal", "unsupported", "unsupported-mode", "upstream"},
	"eventReasons":              {"", "ack-invalid", "approval-required", "authorization", "cancelled", "capacity", "client-mismatch", "context", "deadline", "detached", "identity-conflict", "identity-mismatch", "invalid", "invalid-token", "lease-replaced", "network", "not-activated", "not-found", "other", "owned", "owned-local", "owner-mismatch", "project-deleted", "project-deleting", "project-disabled", "protocol", "refused", "registration-in-use", "revision-mismatch", "secret-mismatch", "sink", "stale-probe", "still-receiving", "storage", "suspended", "terminal", "unsupported-mode", "upstream"},
	"activityReasons":           {"budget-exceeded", "evidence-missing", "identity-mismatch", "malformed-evidence", "observer-unavailable", "stale-evidence", "unsupported"},
	"errorClasses":              {"", "ack-invalid", "authorization", "cancelled", "deadline", "lease-replaced", "network", "not-activated", "other", "project-deleted", "project-deleting", "project-disabled", "protocol"},
	"authStates":                {"", "authorized", "denied", "expired", "failed", "pending", "rejected", "unauthorized"},
	"failureCodes":              {"", "ack-invalid", "authorization", "cancelled", "capacity", "deadline", "detached", "identity-mismatch", "invalid", "invalid-token", "lease-replaced", "network", "not-activated", "not-found", "other", "owned", "owned-local", "project-deleted", "project-deleting", "project-disabled", "protocol", "registration-in-use", "revision-mismatch", "stale-probe", "storage", "suspended"},
	"failureReasons":            {"", "context", "probe-failed", "receive-failed", "reconnect-failed", "renew-failed", "sink", "sink-failed", "terminal", "upstream"},
	"summaryClasses":            {"", "ack-invalid", "authorization", "cancelled", "deadline", "lease-replaced", "network", "not-activated", "other", "project-deleted", "project-deleting", "project-disabled", "protocol"},
	"receiverExits":             {"", "context", "sink", "terminal", "upstream"},
	"summaryAuthStates":         {"", "authorized", "denied", "expired", "failed", "pending", "rejected", "unauthorized"},
}

func TestDiagnosticAllowlistsMatchFrozenAcceptSets(t *testing.T) {
	frozen := func(name, value string) bool { return slices.Contains(frozenDiagnosticAccept[name], value) }
	universe := []string{"", "unknown", "x", "OTHER", " other", "other ", "renew-success", "device-identity", "activity-unavailable", "secret-created-lock-present"}
	for _, values := range frozenDiagnosticAccept {
		universe = append(universe, values...)
	}
	for _, reason := range universe {
		want := knownDeviceIdentityReason(reason) || frozen("knownReasonsWithoutDevice", reason)
		if got := knownDiagnosticReason(reason); got != want {
			t.Fatalf("knownDiagnosticReason(%q) = %v, want %v", reason, got, want)
		}
		for name, got := range map[string]bool{
			"failureCodes":      validSummary(DiagnosticSummary{LastFailureCode: reason}),
			"failureReasons":    validSummary(DiagnosticSummary{LastFailureReason: reason}),
			"summaryClasses":    validSummary(DiagnosticSummary{LastFailureClass: reason}),
			"receiverExits":     validSummary(DiagnosticSummary{LastReceiverExit: reason}),
			"summaryAuthStates": validSummary(DiagnosticSummary{AuthState: reason}),
		} {
			if want := frozen(name, reason); got != want {
				t.Fatalf("validSummary %s %q = %v, want %v", name, reason, got, want)
			}
		}
	}
	base := DiagnosticEvent{At: time.Unix(1, 0), SessionID: "00000000-0000-4000-8000-000000000000", Event: "renew-failed"}
	if !validDiagnostic(base) {
		t.Fatal("base event must be valid")
	}
	events := append([]string{""}, universe...)
	for _, event := range events {
		for _, reason := range universe {
			e := base
			e.Event, e.Reason = event, reason
			var reasonOK bool
			switch event {
			case "activity-unavailable":
				reasonOK = frozen("activityReasons", reason)
			case "device-identity":
				reasonOK = knownDeviceIdentityReason(reason)
			default:
				reasonOK = frozen("eventReasons", reason)
			}
			if got, want := validDiagnostic(e), knownDiagnosticEvent(event) && reasonOK; got != want {
				t.Fatalf("validDiagnostic(event=%q reason=%q) = %v, want %v", event, reason, got, want)
			}
		}
	}
	for _, value := range universe {
		e := base
		e.ErrorClass = value
		if got, want := validDiagnostic(e), frozen("errorClasses", value); got != want {
			t.Fatalf("validDiagnostic errorClass %q = %v, want %v", value, got, want)
		}
		e = base
		e.AuthState = value
		if got, want := validDiagnostic(e), frozen("authStates", value); got != want {
			t.Fatalf("validDiagnostic authState %q = %v, want %v", value, got, want)
		}
	}
}
