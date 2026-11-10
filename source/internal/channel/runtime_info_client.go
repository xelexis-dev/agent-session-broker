package channel

type runtimeInfoUpstream interface{ runtimeInfoAllowed() bool }

func (u *receiveUpstream) runtimeInfoAllowed() bool {
	v, ok := u.Upstream.(runtimeInfoUpstream)
	return ok && v.runtimeInfoAllowed()
}
