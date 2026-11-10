package protocol

// RebindRequest is an internal, explicitly approved conversation handover.
// It creates no registration and carries no project token.
type RebindRequest struct {
	Holder           *ReceiverHolder `json:"holder,omitempty"`
	HolderProof      *HolderProof    `json:"holderProof,omitempty"`
	RegistrationID   string          `json:"registrationId"`
	Harness          string          `json:"harness"`
	SessionID        string          `json:"sessionId"`
	ClientID         string          `json:"clientId"`
	PID              int             `json:"pid"`
	CWD              string          `json:"cwd"`
	ProcessKey       string          `json:"processKey,omitempty"`
	Secret           string          `json:"secret"`
	RequestID        string          `json:"requestId"`
	ExpectedRevision uint64          `json:"expectedRevision"`
}

func (r RebindRequest) RegistrationRequest() RegisterRequest {
	return RegisterRequest{Holder: r.Holder, HolderProof: r.HolderProof, Harness: r.Harness, SessionID: r.SessionID, ClientID: r.ClientID, PID: r.PID, CWD: r.CWD, ProcessKey: r.ProcessKey, Secret: r.Secret, RequestID: r.RequestID, ExpectedRevision: r.ExpectedRevision}
}

type RebindResult struct {
	HolderChallenge *HolderChallenge `json:"holderChallenge,omitempty"`
	RegistrationID  string           `json:"registrationId,omitempty"`
	Name            string           `json:"name,omitempty"`
	Revision        uint64           `json:"revision,omitempty"`
	HandoverFrom    *HandoverFrom    `json:"handoverFrom,omitempty"`
	Reason          string           `json:"reason,omitempty"`
}

// RebindJournal contains only server-observed proof metadata, not resume
// secrets or raw identities. It is never an MCP tool argument or output.
type RebindJournal struct {
	RequestID         string `json:"requestId"`
	TokenID           string `json:"tokenId"`
	SourceDigest      string `json:"sourceDigest"`
	RegistrationID    string `json:"registrationId"`
	ExpectedRevision  uint64 `json:"expectedRevision"`
	TemporaryNickname string `json:"temporaryNickname"`
	Name              string `json:"name"`
}
