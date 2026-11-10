package controlpanel

import "github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"

// Inspect the persisted SQLite payload rather than a synthetic JSON backend.
func readStoredControlState(directory string) ([]byte, error) {
	db, err := sqlitestore.Open(directory, "control.db")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return db.Get("state")
}
