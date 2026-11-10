package broker

import "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"

// InspectRebindResult proves the exact apply for a pending server-side journal.
// Startup recovery uses this without exposing a resume secret or identity.
func (s *RemoteService) InspectRebindResult(id, request, caller string, expected uint64) (protocol.RebindResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byRegistration[id]
	if e == nil || e.record.LastRequestID != request || e.record.LastRequestRevision != expected || e.record.Revision != expected+1 || e.record.AdmissionGrantID != caller || e.record.HandoverFrom == nil {
		return protocol.RebindResult{}, false
	}
	return protocol.RebindResult{RegistrationID: id, Name: e.record.Name, Revision: e.record.Revision, HandoverFrom: e.record.HandoverFrom}, true
}
