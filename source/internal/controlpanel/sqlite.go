package controlpanel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/xelexis-dev/agent-session-broker/source/internal/sqlitestore"
)

// Expiry commits state before notifying watchers, so a panic cannot safely retry.
const expirySweepPolicy = serverdiag.BackgroundStop

// OpenSQLite is the production control store. Legacy JSON is imported once
// without rewriting it; all subsequent mutations are SQLite transactions.
func OpenSQLite(directory string) (*Store, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, ErrStorage
	}
	s, err := openControlRoot(absolute)
	if err != nil {
		return nil, err
	}
	fail := func() (*Store, error) { s.Close(); return nil, ErrStorage }
	pin, err := s.root.Open(".")
	if err != nil {
		return fail()
	}
	path := absolute
	switch runtime.GOOS {
	case "linux":
		path = fmt.Sprintf("/proc/self/fd/%d", pin.Fd())
	case "darwin":
		path = fmt.Sprintf("/dev/fd/%d", pin.Fd())
	}
	database, err := sqlitestore.Open(path, "control.db")
	if err != nil {
		pin.Close()
		return fail()
	}
	s.closeDatabase = func() error { return errors.Join(database.Close(), pin.Close()) }
	s.write = func(data []byte) error { return database.Put("state", data) }
	s.writeBatch = database.Batch
	s.listRecords = database.List
	records, apiErr := database.List()
	if apiErr != nil {
		return fail()
	}
	s.loadAPI(records)
	s.audits = map[string]AuditEvent{}
	initialized, err := database.Initialized()
	if err != nil {
		return fail()
	}
	if !initialized {
		initial, err := json.Marshal(s.data)
		if err != nil {
			return fail()
		}
		records := map[string][]byte{"state": initial}
		info, e := s.root.Lstat("control.json")
		if e == nil {
			if !info.Mode().IsRegular() || info.Size() > controlMaxBytes {
				return fail()
			}
			f, e := s.root.Open("control.json")
			if e != nil {
				return fail()
			}
			raw, e := io.ReadAll(io.LimitReader(f, controlMaxBytes+1))
			f.Close()
			if e != nil || decodeControlState(raw, &s.data) != nil {
				return fail()
			}
			records["state"] = raw
		} else if !errors.Is(e, os.ErrNotExist) {
			return fail()
		}
		if database.Initialize(records) != nil {
			return fail()
		}
	}
	raw, err := database.Get("state")
	if err == nil {
		if decodeSQLiteControlState(raw, &s.data) != nil {
			return fail()
		}
	} else {
		return fail()
	}
	if raw, err := database.Get(attachmentSettingKey); err == nil {
		if setting := decodeAttachmentSetting(raw); setting != nil {
			s.attachmentEnabled = bool(*setting.Enabled)
			s.attachmentLastChange = setting
		}
	}
	if err := s.loadAuditRecords(database); err != nil {
		return fail()
	}
	s.loadLanguages(records)
	if err := s.migrateControlState(); err != nil {
		return fail()
	}
	if err := s.prepareSetupCode(); err != nil {
		return fail()
	}
	serverdiag.StartBackground(expirySweepPolicy, s.stop, s.sweepLoop, nil)
	return s, nil
}

func decodeSQLiteControlState(raw []byte, target *state) error {
	candidate, err := parseControlState(raw)
	if err != nil {
		return err
	}
	if !validProjectGenerations(candidate) || !validScopePolicy(candidate) || !validAutoState(candidate) {
		return ErrStorage
	}
	if !validState(candidate) && !((candidate.Version == 2 || candidate.Version == scopeIntroductionVersion || candidate.Version == controlVersion) && candidate.Admin == nil && candidate.Projects != nil && len(candidate.Projects) == 0 && len(candidate.Users) == 0) {
		return ErrStorage
	}
	*target = candidate
	return nil
}

func parseControlState(raw []byte) (state, error) {
	var candidate state
	if len(raw) > controlMaxBytes {
		return candidate, ErrStorage
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&candidate) != nil {
		return state{}, ErrStorage
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return state{}, ErrStorage
	}
	if candidate.Version < scopeIntroductionVersion && len(raw) > legacyControlMaxBytes {
		return state{}, ErrStorage
	}
	return candidate, nil
}

func decodeControlState(raw []byte, target *state) error {
	candidate, err := parseControlState(raw)
	if err != nil || !validState(candidate) {
		return ErrStorage
	}
	*target = candidate
	return nil
}
