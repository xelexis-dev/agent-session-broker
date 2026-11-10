package controlpanel

import (
	"errors"
	"time"
)

const (
	projectTokenCap       = 256
	tokenHistoryRetention = 30 * 24 * time.Hour
)

var ErrTokenCap = errors.New("project active token limit reached (256); revoke unused tokens or detach old sessions before issuing or approving another token")

func tokenActive(token SessionToken, now time.Time) bool {
	return !token.Revoked && !tokenExpired(token, now)
}

func activeTokenCount(tokens []SessionToken, now time.Time) int {
	n := 0
	for _, token := range tokens {
		if tokenActive(token, now) {
			n++
		}
	}
	return n
}

// Old state has no revocation timestamp. Start its retention on migration,
// rather than guessing a past date and prematurely deleting history.
func (s *Store) migrateRevokedHistory() error {
	for _, p := range s.data.Projects {
		for _, token := range p.Tokens {
			if token.Revoked && token.RevokedAt.IsZero() {
				return s.commit(s.data)
			}
		}
	}
	return nil
}
