package serverruntime

import (
	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
	"testing"
)

func readControlFixture(t *testing.T, directory string) []byte {
	t.Helper()
	db, err := sqlitestore.Open(directory, "control.db")
	if err != nil {
		t.Fatal("control fixture DB open failed")
	}
	defer db.Close()
	raw, err := db.Get("state")
	if err != nil {
		t.Fatal("control fixture state read failed")
	}
	return raw
}

func writeControlFixture(t *testing.T, directory string, raw []byte) {
	t.Helper()
	db, err := sqlitestore.Open(directory, "control.db")
	if err != nil {
		t.Fatal("control fixture DB open failed")
	}
	defer db.Close()
	if db.Put("state", raw) != nil {
		t.Fatal("control fixture state write failed")
	}
}
