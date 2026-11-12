package controlpanel

import (
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"net/http"
	"net/netip"
	"time"
)

var errHTTPNotAllowed = errors.New("http_not_allowed")

type requestTransport struct {
	secure, plaintext bool
	forwardedHint     bool
	peer, reason      string
}

// Only an explicitly trusted peer can attest to the original scheme. A trusted
// proxy must SET one XFP value; upstream TLS cannot repair a missing attestation.
func (p *panel) transport(r *http.Request) requestTransport {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	ip := peer.Addr().Unmap()
	out := requestTransport{secure: r.TLS != nil, peer: "unknown"}
	trusted := false
	if err == nil && ip.Zone() == "" {
		out.peer = ip.String()
		for _, prefix := range p.trustedProxies {
			if prefix.Contains(ip) {
				trusted = true
				break
			}
		}
	}
	forwarded := r.Header.Values("X-Forwarded-Proto")
	if trusted {
		out.secure = len(forwarded) == 1 && forwarded[0] == "https"
	}
	out.forwardedHint = !trusted && len(forwarded) > 0
	out.plaintext = r.Header.Get("X-ASB-Client-Transport") == "http" || !out.secure
	switch {
	case r.Header.Get("X-ASB-Client-Transport") == "http":
		out.reason = "marked"
	case !trusted && len(forwarded) > 0:
		out.reason = "untrusted-forwarded"
	default:
		out.reason = "unproven"
	}
	return out
}
func (p *panel) secure(r *http.Request) bool    { return p.transport(r).secure }
func (p *panel) plaintext(r *http.Request) bool { return p.transport(r).plaintext }

func (p *panel) denyTransport(w http.ResponseWriter, r *http.Request, project string) {
	verdict := p.transport(r)
	now := time.Now()
	p.mu.Lock()
	if p.transportLogs == nil {
		p.transportLogs = map[string]time.Time{}
	}
	prior := p.transportLogs[verdict.reason]
	report := now.Sub(prior) >= time.Minute
	if report {
		p.transportLogs[verdict.reason] = now
	}
	p.mu.Unlock()
	if report {
		serverdiag.Emit(serverdiag.Event{Type: "http-transport-denied", Reason: verdict.reason, Route: AccessRoute(r), Peer: verdict.peer, ForwardedHint: verdict.forwardedHint})
	}
	writeTransportDenied(w)
}
