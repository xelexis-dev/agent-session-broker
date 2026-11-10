package clientstate

// Frozen validators copied from tag 0.17.2 for rollback compatibility.
func v0172ValidDiagnostic(e DiagnosticEvent) bool {
	activityReason := e.Event == "activity-unavailable" && diagnosticChoice(e.Reason, "unsupported", "evidence-missing", "identity-mismatch", "malformed-evidence", "stale-evidence", "budget-exceeded", "observer-unavailable")
	return !e.At.IsZero() && diagnosticSessionID.MatchString(e.SessionID) && (e.RegistrationID == "" || diagnosticRegistrationID.MatchString(e.RegistrationID)) &&
		diagnosticChoice(e.Event, "renew-success", "renew-failed", "receive-failed", "sink-failed", "receiver-exit", "reconnect-start", "reconnect-failed", "reconnected", "auth-transition", "transport-created", "transport-replaced", "probe-failed", "activity-unavailable", "token-changed", "legacy-migration", "handover-skipped", "follow-message-dropped", "codex-stop-missing", "kimi-hook-handoff") &&
		(activityReason || (e.Event != "activity-unavailable" && diagnosticChoice(e.Reason, "approval-required", "still-receiving", "secret-mismatch", "client-mismatch", "owner-mismatch", "unsupported-mode", "identity-conflict", "refused", "", "upstream", "context", "sink", "terminal", "invalid", "not-found", "detached", "suspended", "owned", "owned-local", "registration-in-use", "revision-mismatch", "stale-probe", "storage", "invalid-token", "capacity", "project-deleted", "project-disabled", "project-deleting", "identity-mismatch", "deadline", "cancelled", "network", "authorization", "protocol", "other"))) &&
		diagnosticChoice(e.ErrorClass, "", "project-deleted", "project-disabled", "project-deleting", "deadline", "cancelled", "network", "authorization", "protocol", "other") &&
		diagnosticChoice(e.AuthState, "", "unauthorized", "pending", "authorized", "denied", "expired", "rejected", "failed")
}
func v0172ValidSummary(s DiagnosticSummary) bool {
	return diagnosticChoice(s.LastFailureCode, "", "invalid", "not-found", "detached", "suspended", "owned", "owned-local", "registration-in-use", "revision-mismatch", "stale-probe", "storage", "invalid-token", "capacity", "project-deleted", "project-disabled", "project-deleting", "identity-mismatch", "deadline", "cancelled", "network", "authorization", "protocol", "other") && diagnosticChoice(s.LastFailureReason, "", "renew-failed", "receive-failed", "sink-failed", "reconnect-failed", "probe-failed", "upstream", "context", "sink", "terminal") &&
		diagnosticChoice(s.LastFailureClass, "", "project-deleted", "project-disabled", "project-deleting", "deadline", "cancelled", "network", "authorization", "protocol", "other") &&
		diagnosticChoice(s.LastReceiverExit, "", "upstream", "context", "sink", "terminal") &&
		diagnosticChoice(s.AuthState, "", "unauthorized", "pending", "authorized", "denied", "expired", "rejected", "failed")
}
