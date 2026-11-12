package accesslog

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"net/netip"
	"sync/atomic"
)

type Record struct {
	Type             string `json:"type"`
	Time             string `json:"time,omitempty"`
	Method           string `json:"method,omitempty"`
	Route            string `json:"route,omitempty"`
	PathLen          *int   `json:"path_len,omitempty"`
	Status           int    `json:"status"`
	DurationMS       int64  `json:"duration_ms"`
	Bytes            int64  `json:"bytes"`
	Client           string `json:"client,omitempty"`
	ClientSource     string `json:"client_source,omitempty"`
	Peer             string `json:"peer,omitempty"`
	TLS              bool   `json:"tls"`
	ForwardedIgnored bool   `json:"forwarded_ignored,omitempty"`
	Aborted          bool   `json:"aborted,omitempty"`
	Panic            bool   `json:"panic,omitempty"`
	ProcessStartedAt string `json:"process_started_at"`
	Counters
	diagnostic *serverdiag.Event
}
type Counters struct {
	Dropped        uint64 `json:"dropped_total,omitempty"`
	WriteErrors    uint64 `json:"write_errors_total,omitempty"`
	RotationErrors uint64 `json:"rotation_errors_total,omitempty"`
	PruneErrors    uint64 `json:"prune_errors_total,omitempty"`
	Reopens        uint64 `json:"reopens_total,omitempty"`
}
type totals struct{ dropped, writeErrors, rotationErrors, pruneErrors, reopens atomic.Uint64 }

func (t *totals) snapshot() Counters {
	return Counters{t.dropped.Load(), t.writeErrors.Load(), t.rotationErrors.Load(), t.pruneErrors.Load(), t.reopens.Load()}
}
func safeIP(value string) string {
	ip, err := netip.ParseAddr(value)
	if err != nil || ip.Zone() != "" {
		return "unknown"
	}
	return ip.Unmap().String()
}
