package serverruntime

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"net/http"
)

// Protection stays enabled independently of the optional access log.
func protectRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v != http.ErrAbortHandler {
					serverdiag.Emit(serverdiag.Panic(v))
				}
				panic(http.ErrAbortHandler)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
