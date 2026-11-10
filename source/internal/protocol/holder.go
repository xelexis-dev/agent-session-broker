package protocol

// ReceiverHolder identifies the receiving client, never its parent harness.
// Incarnation and DeviceID are domain-separated opaque HMACs, not machine IDs.
type ReceiverHolder struct {
	DeviceID    string `json:"deviceId"`
	PID         int    `json:"pid"`
	Incarnation string `json:"incarnation"`
}

// HolderChallenge is disclosed only after authenticated registration proof.
type HolderChallenge struct {
	Holder     ReceiverHolder `json:"holder"`
	Revision   uint64         `json:"revision"`
	Generation uint64         `json:"generation"`
	Nonce      string         `json:"nonce"`
}
type HolderProof struct {
	Challenge HolderChallenge `json:"challenge"`
	Dead      bool            `json:"dead"`
}

const ReceiverHolderCapability = "receiver-holder-v1"

const CodeRegistrationInUse = "registration-in-use"

func ValidReceiverHolder(h *ReceiverHolder) bool {
	if h == nil {
		return true
	}
	return h.PID > 0 && opaqueHolderID(h.DeviceID) && opaqueHolderID(h.Incarnation)
}
func opaqueHolderID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
