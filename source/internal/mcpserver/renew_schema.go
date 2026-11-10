package mcpserver

// The SDK infers json.RawMessage as []byte. Spell out this small input schema
// so any optional JSON activity reaches the authorized bounded decoder. The
// typed handler still enforces the existing heartbeat envelope.
func renewInputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"registrationId", "channelId"},
		"properties": map[string]any{
			"epoch":          map[string]any{"type": "string"},
			"ackThrough":     map[string]any{"type": "integer", "minimum": 0},
			"registrationId": map[string]any{"type": "string"},
			"channelId":      map[string]any{"type": "string"},
			"ingressPending": map[string]any{"type": "boolean"},
			"activity":       map[string]any{},
			"runtimeInfo":    map[string]any{},
		},
	}
}
