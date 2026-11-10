package protocol

// RenameRequest is the client-to-server request. The local client supplies
// the registration and lease; the model only chooses Name.
type RenameRequest struct {
	Name           string `json:"name" jsonschema:"New communication nickname for this connected session; duplicates are rejected"`
	RegistrationID string `json:"registrationId"`
	ChannelID      string `json:"channelId"`
}
