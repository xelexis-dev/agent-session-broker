package protocol

import (
	"encoding/json"
	"errors"
)

const RecipientExpectationCapability = "message.recipient-registration"
const CodeRecipientChanged = "recipient-changed"
const CodeUnsupported = "unsupported"

func ValidRegistrationID(id string) bool {
	if len(id) != 26 {
		return false
	}
	for _, c := range id {
		if !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}
func ValidRecipientExpectation(id *string) bool { return id == nil || ValidRegistrationID(*id) }

// Inspect the raw field so explicit null cannot become an absent expectation.
func RecipientExpectation(raw []byte) (*string, error) {
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil {
		return nil, errors.New("invalid expectRegistrationId")
	}
	v, present := args["expectRegistrationId"]
	if !present {
		return nil, nil
	}
	var id string
	if json.Unmarshal(v, &id) != nil || !ValidRegistrationID(id) {
		return nil, errors.New("invalid expectRegistrationId")
	}
	return &id, nil
}
