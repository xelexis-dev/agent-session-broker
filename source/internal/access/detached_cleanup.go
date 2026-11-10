package access

// DetachedRemovalProof is server-owned evidence captured under the broker lock.
// It is never transported to a client or logged as a raw binding identity.
type DetachedRemovalProof struct {
	RegistrationID, Identity, OwnerID, Name string
	AdmissionMode, GrantID, ParentID        string
	Revision                                uint64
	Detached, ReattachAfterCleanup          bool
}
