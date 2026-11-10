package broker

// RegistrationIdentity is the string handed to admission binding callbacks.
func RegistrationIdentity(harness, sessionID, clientID string) string {
	return harness + ":" + sessionID + ":" + clientID
}
