package access

import "time"

// ScopeDetail is shared by token issuance and credential diagnostics.
type ScopeDetail struct {
	Scope       string `json:"scope"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// ScopeCatalog returns a copy in canonical scope order. Descriptions describe
// permission checks; identity/ownership checks still apply independently.
func ScopeCatalog() []ScopeDetail {
	return []ScopeDetail{
		{MemoryRead, "Read memory", "Read project memory with memory_get and memory_search."},
		{MemoryWrite, "Write memory", "Save project memory with memory_put."},
		{MessageReceive, "Receive messages", "Receive messages for this session with wait_for_message."},
		{MessageSend, "Send messages", "Send messages from this registered session to other sessions with send_message."},
		{SessionManage, "Manage sessions", "Disconnect this session with stop_session. Use stop_session, retire_session, suspend_session and unsuspend_session to disconnect, retire, suspend or resume other sessions the token owner can manage. retire_session retires the registration and revokes the session credentials it used, releasing the name reservation. Retiring, suspending or resuming this session itself is not allowed. Tokens owned by an enabled instance administrator can use list_bug_reports, get_bug_report and set_bug_report_status to read and handle bug reports in the current project generation."},
		{SessionRead, "Read sessions", "Read project sessions with list_sessions and get_session."},
		{SessionRegister, "Register and stay connected (required)", "Register this session and maintain its connection with attach_session, register_session, resume_session, renew_lease, probe_response and report_ingress_failure."},
		{SessionRegisterAuto, "Register without approval on request", "Register without approval when the user asks to connect this conversation. Starting a new conversation alone does not register it. Request direct registration with a project token without human approval; this permission is not delegated to session credentials."},
		{SessionRename, "Rename this session", "Change this session's communication name with rename_session."},
	}
}

func ScopeDetails(scopes []string) []ScopeDetail {
	out := make([]ScopeDetail, 0, len(scopes))
	for _, d := range ScopeCatalog() {
		if HasScope(scopes, d.Scope) {
			out = append(out, d)
		}
	}
	return out
}

// TokenInfoTool is callable for diagnostics but never advertised to a host.
const TokenInfoTool = "session_token_info"

type TokenInfo struct {
	ProjectGenerationID string    `json:"projectGenerationId,omitempty"`
	ProjectRefusals     bool      `json:"projectRefusals,omitempty"`
	Scopes              []string  `json:"scopes"`
	AdmissionMode       string    `json:"admissionMode"`
	ExpiresAt           time.Time `json:"expiresAt,omitzero"`
}
