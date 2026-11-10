package clientstate

import (
	"encoding/json"
	"errors"
	"regexp"
)

var diagnosticCode = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

// Only storage retains extension fields; output callers use SafeDiagnostics.
func diagnosticObject(raw []byte, dst any, keys ...string) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(raw, dst); err != nil {
		return nil, err
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(raw, &extra); err != nil {
		return nil, err
	}
	if extra == nil {
		return nil, errors.New("invalid diagnostic object")
	}
	for _, key := range keys {
		delete(extra, key)
	}
	return extra, nil
}
func marshalDiagnosticObject(value any, extra map[string]json.RawMessage) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(extra) == 0 {
		return raw, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for key, value := range extra {
		fields[key] = value
	}
	return json.Marshal(fields)
}
func (e *DiagnosticEvent) UnmarshalJSON(raw []byte) (err error) {
	type plain DiagnosticEvent
	e.extra, err = diagnosticObject(raw, (*plain)(e), "at", "sessionId", "registrationId", "generation", "event", "reason", "errorClass", "authState")
	return
}
func (e DiagnosticEvent) MarshalJSON() ([]byte, error) {
	type plain DiagnosticEvent
	return marshalDiagnosticObject(plain(e), e.extra)
}
func (s *DiagnosticSummary) UnmarshalJSON(raw []byte) (err error) {
	type plain DiagnosticSummary
	s.extra, err = diagnosticObject(raw, (*plain)(s), "handoverSkipped", "unavailable", "lastRenewSuccess", "lastFailureAt", "lastFailureReason", "lastFailureCode", "lastFailureClass", "lastReceiverExit", "authState", "unknownCodes")
	return
}
func (s DiagnosticSummary) MarshalJSON() ([]byte, error) {
	type plain DiagnosticSummary
	return marshalDiagnosticObject(plain(s), s.extra)
}
func (d *Diagnostics) UnmarshalJSON(raw []byte) (err error) {
	var fields struct {
		Format  int               `json:"format"`
		Summary DiagnosticSummary `json:"summary"`
		Events  []json.RawMessage `json:"events"`
	}
	d.extra, err = diagnosticObject(raw, &fields, "format", "summary", "events")
	if err != nil {
		return err
	}
	d.Format, d.Summary, d.Events = fields.Format, fields.Summary, nil
	for _, raw := range fields.Events {
		var event DiagnosticEvent
		if json.Unmarshal(raw, &event) == nil && validStoredDiagnostic(event) {
			d.Events = append(d.Events, event)
		}
	}
	return nil
}
func (d Diagnostics) MarshalJSON() ([]byte, error) {
	type plain Diagnostics
	return marshalDiagnosticObject(plain(d), d.extra)
}
func storedCodes(codes ...string) bool {
	for _, code := range codes {
		if code != "" && !diagnosticCode.MatchString(code) {
			return false
		}
	}
	return true
}
func validStoredDiagnostic(e DiagnosticEvent) bool {
	return !e.At.IsZero() && diagnosticSessionID.MatchString(e.SessionID) && (e.RegistrationID == "" || diagnosticRegistrationID.MatchString(e.RegistrationID)) && diagnosticCode.MatchString(e.Event) && storedCodes(e.Reason, e.ErrorClass, e.AuthState)
}

// SafeDiagnostics projects only known typed fields, never extension data or raw codes.
func SafeDiagnostics(d Diagnostics) Diagnostics {
	out := Diagnostics{Summary: d.Summary}
	out.Summary.extra = nil
	out.Summary.UnknownCodes = 0
	safe := func(value string, allowed bool) string {
		if value == "" || allowed {
			return value
		}
		out.Summary.UnknownCodes++
		return "unknown"
	}
	s := &out.Summary
	s.HandoverSkipped = safe(s.HandoverSkipped, knownDiagnosticReason(s.HandoverSkipped))
	s.LastFailureCode = safe(s.LastFailureCode, validSummary(DiagnosticSummary{LastFailureCode: s.LastFailureCode}))
	s.LastFailureReason = safe(s.LastFailureReason, validSummary(DiagnosticSummary{LastFailureReason: s.LastFailureReason}))
	s.LastFailureClass = safe(s.LastFailureClass, validSummary(DiagnosticSummary{LastFailureClass: s.LastFailureClass}))
	s.LastReceiverExit = safe(s.LastReceiverExit, validSummary(DiagnosticSummary{LastReceiverExit: s.LastReceiverExit}))
	s.AuthState = safe(s.AuthState, validSummary(DiagnosticSummary{AuthState: s.AuthState}))
	for _, event := range d.Events {
		event.extra = nil
		event.Event = safe(event.Event, knownDiagnosticEvent(event.Event))
		event.Reason = safe(event.Reason, knownDiagnosticReason(event.Reason))
		event.ErrorClass = safe(event.ErrorClass, validSummary(DiagnosticSummary{LastFailureClass: event.ErrorClass}))
		event.AuthState = safe(event.AuthState, validSummary(DiagnosticSummary{AuthState: event.AuthState}))
		out.Events = append(out.Events, event)
	}
	return out
}
