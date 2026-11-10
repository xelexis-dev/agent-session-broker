package controlpanel

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/access"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
)

const directPath = "/oauth/session_registration"
const capabilityPath = "/oauth/capabilities"

type directInspector interface {
	InspectDirectRegistration(context.Context, string, DirectRequest) (DirectProof, error)
}

func (p *panel) directHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == capabilityPath {
		if r.Method != "GET" {
			w.Header().Set("Allow", "GET")
			writeDeviceError(w, 405, "invalid_request")
			return
		}
		capabilities := []string{"scoped-session-v1", "harness-kimi-v1"}
		if p.store.scopedIssuanceReady() {
			capabilities = append(capabilities, "direct-registration-v1", protocol.ReceiverHolderCapability)
		}
		writeDeviceJSON(w, 200, map[string]any{"version": 1, "capabilities": capabilities})
		return
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		writeDeviceError(w, 405, "invalid_request")
		return
	}
	if !p.store.scopedIssuanceReady() {
		writeDeviceError(w, 403, string(errDirectUnavailable))
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	projects := query["project"]
	if err != nil || len(query) != 1 || len(projects) != 1 || !projectName.MatchString(projects[0]) {
		writeDeviceError(w, 400, "invalid_request")
		return
	}
	project := projects[0]
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.ParseForm() != nil {
		writeDeviceError(w, 400, "invalid_request")
		return
	}
	for key, values := range r.PostForm {
		switch key {
		case "client_id", "harness", "session_id", "asb_client_instance_id", "request_id", "requested_nickname", "registration_id", "resume_secret", "handover", "pid", "cwd", "process_key", "holder", "holder_proof":
		default:
			writeDeviceError(w, 400, "invalid_request")
			return
		}
		if len(values) != 1 {
			writeDeviceError(w, 400, "invalid_request")
			return
		}
	}
	form := r.PostForm
	request := DirectRequest{DeviceRequest: DeviceRequest{ClientID: form.Get("client_id"), Harness: form.Get("harness"), SessionID: form.Get("session_id"), InstanceID: form.Get("asb_client_instance_id"), RequestID: form.Get("request_id")}, RequestedNickname: form.Get("requested_nickname"), RegistrationID: form.Get("registration_id"), ResumeSecret: form.Get("resume_secret")}
	request.Handover = form.Get("handover") == "true"
	request.PID, _ = strconv.Atoi(form.Get("pid"))
	request.CWD = form.Get("cwd")
	request.ProcessKey = form.Get("process_key")
	if raw := form.Get("holder"); raw != "" {
		if json.Unmarshal([]byte(raw), &request.Holder) != nil || request.Holder == nil || !protocol.ValidReceiverHolder(request.Holder) {
			writeDeviceError(w, 400, "invalid_request")
			return
		}
	}
	if raw := form.Get("holder_proof"); raw != "" {
		if json.Unmarshal([]byte(raw), &request.HolderProof) != nil || request.HolderProof == nil {
			writeDeviceError(w, 400, "invalid_request")
			return
		}
	}
	if form.Get("handover") != "" && !request.Handover {
		writeDeviceError(w, 400, "invalid_request")
		return
	}
	if !request.valid() {
		writeDeviceError(w, 400, "invalid_request")
		return
	}
	raw := bearerToken(r)
	if raw == "" {
		writeDeviceError(w, 401, "invalid_client")
		return
	}
	plaintext := p.plaintext(r)
	if plaintext && !p.store.AllowHTTP(project) {
		p.denyTransport(w, r, project)
		return
	}
	peer, _, peerErr := net.SplitHostPort(r.RemoteAddr)
	if peerErr != nil {
		peer = r.RemoteAddr
	}
	if err = p.store.directPreflight(project, raw); err != nil {
		if reason := directDenialReason(err); reason != "" {
			p.store.recordDirectDenial(project, raw, request, peer, reason)
		}
		writeDirectFailure(w, err)
		return
	}
	var proof DirectProof
	if backend, ok := p.backend.(directInspector); ok {
		proof, err = backend.InspectDirectRegistration(r.Context(), project, request)
	} else {
		proof.Absent = true
		var sessions []Session
		sessions, err = p.backend.Sessions(r.Context(), project)
		for _, session := range sessions {
			proof.Names = append(proof.Names, session.Name)
			if sessionIdentity(session) == request.identity() {
				proof.Absent = false
				err = errDirectConflict
			}
		}
		if request.RegistrationID != "" {
			err = errDirectConflict
		}
	}
	if err != nil {
		p.store.recordDirectDenial(project, raw, request, peer, "conflict")
		var refusal protocol.HandoverRefusal
		if request.Handover || (errors.As(err, &refusal) && refusal.Reason == protocol.CodeNotFound) {
			writeHandoverFailure(w, err)
			return
		}
		writeDirectFailure(w, errDirectConflict)
		return
	}
	if request.Handover && request.RequestedNickname != "" && request.RequestedNickname != proof.Name {
		writeHandoverFailure(w, protocol.HandoverRefusal{Reason: "identity-conflict"})
		return
	}
	if proof.RegistrationID != "" {
		if guard, ok := p.backend.(interface {
			CheckRegistrationHolder(context.Context, string, DirectRequest) error
		}); ok {
			grant, e := p.store.directHolderGrant(project, raw, proof.OwnerID)
			if e == nil {
				e = guard.CheckRegistrationHolder(access.WithGrant(r.Context(), grant), project, request)
			}
			if e != nil {
				writeHandoverFailure(w, e)
				return
			}
		}
	}
	var result DirectResponse
	if fenced, ok := p.backend.(interface {
		WithDirectRegistration(context.Context, string, DirectRequest, func(DirectProof) error) error
	}); ok {
		err = fenced.WithDirectRegistration(r.Context(), project, request, func(final DirectProof) error {
			proof = final
			var exchangeErr error
			result, exchangeErr = p.store.ExchangeDirect(project, raw, request, final, peer, plaintext)
			return exchangeErr
		})
	} else {
		result, err = p.store.ExchangeDirect(project, raw, request, proof, peer, plaintext)
	}
	if errors.Is(err, errHTTPNotAllowed) {
		p.denyTransport(w, r, project)
		return
	}
	if err != nil {
		if reason := directDenialReason(err); reason != "" {
			p.store.recordDirectDenial(project, raw, request, peer, reason)
		}
		if request.Handover {
			writeHandoverFailure(w, err)
		} else {
			writeDirectFailure(w, err)
		}
		return
	}
	if request.Handover {
		rebind, ok := p.backend.(interface {
			RebindConversation(context.Context, string, DirectRequest, DirectProof) (DirectProof, error)
		})
		if !ok {
			writeHandoverFailure(w, protocol.HandoverRefusal{Reason: "unsupported-mode"})
			return
		}
		grant, _, ok := p.store.authorize(project, result.AccessToken)
		if !ok {
			writeHandoverFailure(w, errDirectForbidden)
			return
		}
		updated, e := rebind.RebindConversation(access.WithGrant(r.Context(), grant), project, request, proof)
		if e != nil {
			var refusal protocol.HandoverRefusal
			if errors.As(e, &refusal) {
				if cleanup := p.store.abortConversationHandover(project, grant.TokenID); cleanup != nil {
					writeDirectFailure(w, ErrStorage)
					return
				}
			}
			writeHandoverFailure(w, e)
			return
		}
		if grant.Registration == nil {
			writeDirectFailure(w, ErrStorage)
			return
		}
		if e = grant.Registration(request.identity(), updated.RegistrationID, updated.OwnerID, updated.Name, updated.Revision, true); e != nil {
			writeDirectFailure(w, ErrStorage)
			return
		}
		result.Revision = updated.Revision
		result.HandoverFrom = updated.HandoverFrom
	}
	if proof.RegistrationID != "" && request.RequestedNickname != "" && request.RequestedNickname != proof.Name {
		renamer, ok := p.backend.(interface {
			RenameDirectRegistration(context.Context, string, DirectRequest, DirectProof) (DirectProof, error)
		})
		if !ok {
			writeDirectFailure(w, ErrStorage)
			return
		}
		grant, _, ok := p.store.authorize(project, result.AccessToken)
		if !ok {
			writeDirectFailure(w, errDirectConflict)
			return
		}
		updated, e := renamer.RenameDirectRegistration(access.WithGrant(r.Context(), grant), project, request, proof)
		if e != nil {
			writeDirectFailure(w, errDirectConflict)
			return
		}
		result.Nickname, result.Revision = updated.Name, updated.Revision
	}
	writeDeviceJSON(w, 200, result)
}
func writeDirectFailure(w http.ResponseWriter, err error) {
	status, code := 503, "server_error"
	var device deviceError
	if errors.As(err, &device) {
		code = string(device)
		status = 400
	}
	switch {
	case errors.Is(err, errDeviceClient):
		status = 401
	case errors.Is(err, errDirectForbidden), errors.Is(err, errDirectUnavailable):
		status = 403
	case errors.Is(err, errDirectConflict), errors.Is(err, errDirectCapacity):
		status = 409
	case errors.Is(err, errDirectRate):
		status = 429
		var limited *directRateError
		seconds := 1
		if errors.As(err, &limited) {
			seconds = min(60, max(1, int(math.Ceil(limited.wait.Seconds()))))
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
	}
	writeDeviceError(w, status, code)
}

// Storage/unknown failures have no truthful existing denial reason. Do not
// mislabel them as identity conflicts; their HTTP server error is preserved.
func directDenialReason(err error) string {
	switch {
	case errors.Is(err, errDirectRate):
		return "rate-limited"
	case errors.Is(err, errDirectCapacity):
		return "capacity"
	case errors.Is(err, errDirectConflict):
		return "conflict"
	case errors.Is(err, errDirectForbidden), errors.Is(err, errDirectUnavailable):
		return "forbidden"
	case errors.Is(err, errDeviceClient):
		return "parent-inactive"
	default:
		return ""
	}
}

func writeHandoverFailure(w http.ResponseWriter, err error) {
	var refusal protocol.HandoverRefusal
	if errors.As(err, &refusal) {
		writeDeviceJSON(w, 409, struct {
			Error     string                    `json:"error"`
			Reason    string                    `json:"handover_reason"`
			Challenge *protocol.HolderChallenge `json:"holder_challenge,omitempty"`
		}{"identity_conflict", protocol.SafeHandoverReason(refusal.Reason), refusal.HolderChallenge})
		return
	}
	if errors.Is(err, errDirectForbidden) || errors.Is(err, errDirectUnavailable) {
		writeDeviceJSON(w, 403, map[string]string{"error": "insufficient_scope", "handover_reason": "unsupported-mode"})
		return
	}
	writeDirectFailure(w, err)
}
