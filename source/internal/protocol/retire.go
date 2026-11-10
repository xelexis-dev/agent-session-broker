package protocol

import "time"

type RetireRequest struct {
	RegistrationID string `json:"registrationId,omitempty"`
	Name           string `json:"name,omitempty"`
}
type RevokedCredential struct {
	ID               string    `json:"id"`
	FirstConnectedAt time.Time `json:"firstConnectedAt"`
}
type RetireResult struct {
	OK                 bool                `json:"ok"`
	Mode               string              `json:"mode"`
	Name               string              `json:"name"`
	RegistrationID     string              `json:"registrationId,omitempty"`
	RevokedCredentials []RevokedCredential `json:"revokedCredentials"`
	DiscardedMessages  int                 `json:"discardedMessages"`
}
type RetireRejection struct {
	Code                 string `json:"code"`
	Message              string `json:"message"`
	RegistrationID       string `json:"registrationId,omitempty"`
	HolderRegistrationID string `json:"holderRegistrationId,omitempty"`
}

func (e RetireRejection) Error() string { return e.Code + ": " + e.Message }
func RejectRetire(code string) RetireRejection {
	messages := map[string]string{
		"invalid-selector":          "Supply exactly one registrationId or name.",
		"not-found":                 "No target found. A previous timed-out call may already have completed. Check list_sessions and rename before retrying by name; name mode acts on orphans present at call time.",
		"self-retire-denied":        "Use stop_session and ask another session or the panel to retire this registration.",
		"target-suspended":          "Unsuspend the target before retiring it.",
		"forbidden":                 "The caller cannot manage this target.",
		"name-held-by-registration": "Use the exact registrationId after checking list_sessions.",
		"credential-unused":         "An unused credential reserves this name. Use the panel or wait for expiry.",
		"storage":                   "Retirement could not be stored. No detach was committed.",
		"cleanup-required":          "The target is detached; retry using this registrationId to finish cleanup.",
	}
	message, ok := messages[code]
	if !ok {
		code = "forbidden"
		message = messages[code]
	}
	return RetireRejection{Code: code, Message: message}
}

type RenameRejection struct {
	Code                 string `json:"code"`
	Holder               string `json:"holder"`
	HolderRegistrationID string `json:"holderRegistrationId,omitempty"`
}

func (e RenameRejection) Error() string { return e.Code + ": nickname is reserved (" + e.Holder + ")" }
