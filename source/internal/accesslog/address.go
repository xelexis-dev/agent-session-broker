package accesslog

import (
	"net/http"
	"net/netip"
	"strings"
)

// NormalizeProxies preserves the legacy scheme policy; this set is address-only.
func NormalizeProxies(prefixes []netip.Prefix) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(prefixes))
	for _, p := range prefixes {
		if !p.IsValid() {
			return nil, invalid("configuration")
		}
		if p.Addr().Is4In6() {
			if p.Bits() < 96 {
				return nil, invalid("configuration")
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
func trusted(ip netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

type Address struct {
	Peer, Client, Source string
	Ignored              bool
}

func ClientAddress(r *http.Request, opt bool, prefixes []netip.Prefix) Address {
	out := Address{Peer: "unknown", Client: "unknown", Source: "peer", Ignored: len(r.Header.Values("X-Forwarded-For")) > 0}
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || peer.Addr().Zone() != "" {
		return out
	}
	ip := peer.Addr().Unmap()
	out.Peer = ip.String()
	out.Client = out.Peer
	xfp := r.Header.Values("X-Forwarded-Proto")
	values := r.Header.Values("X-Forwarded-For")
	if !opt || !trusted(ip, prefixes) || len(xfp) != 1 || (xfp[0] != "http" && xfp[0] != "https") || len(values) == 0 {
		return out
	}
	// Inspect only the suffix we need. Padding to the left of the first untrusted
	// hop cannot suppress the directly observed address.
	inspected := 0
	for line := len(values) - 1; line >= 0; line-- {
		value := values[line]
		for {
			if inspected == 32 {
				return out
			}
			inspected++
			cut := strings.LastIndexByte(value, ',')
			part := value
			if cut >= 0 {
				part = value[cut+1:]
			}
			addr, err := netip.ParseAddr(strings.Trim(part, " \t"))
			if err != nil || addr.Zone() != "" {
				return out
			}
			addr = addr.Unmap()
			if !trusted(addr, prefixes) {
				out.Client = addr.String()
				out.Source = "forwarded"
				out.Ignored = false
				return out
			}
			if cut < 0 {
				break
			}
			value = value[:cut]
		}
	}
	return out
}
