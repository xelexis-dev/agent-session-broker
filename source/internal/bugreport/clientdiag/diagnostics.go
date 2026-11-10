// Package clientdiag defines the wire diagnostics shared by clients and the server.
package clientdiag

import "time"

type DiagnosticEvent struct {
	At          time.Time `json:"at"`
	Event       string    `json:"event"`
	ReasonClass string    `json:"reasonClass,omitempty"`
}
type ClientDiagnostics struct {
	ClientVersion string            `json:"clientVersion"`
	ServerVersion string            `json:"serverVersion"`
	Harness       string            `json:"harness"`
	OS            string            `json:"os"`
	Arch          string            `json:"arch"`
	AuthState     string            `json:"authState"`
	ReceiveReady  bool              `json:"receiveReady"`
	LastFollow    string            `json:"lastFollow,omitempty"`
	Events        []DiagnosticEvent `json:"events,omitempty"`
}
