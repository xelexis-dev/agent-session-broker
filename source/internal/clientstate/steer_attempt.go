package clientstate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"time"
)

var ErrSteerRecovery = errors.New("steer recovery unavailable")
var ErrSteerChanged = errors.New("steer attempt changed")

const steerAttemptLimit = 8 * 1024

type SteerAttempt struct {
	ClientID  string   `json:"clientId"`
	Sequences []uint64 `json:"sequences"`
	Turns     []string `json:"turns"`
}
type SteerAttemptRecord struct {
	Format       int           `json:"format"`
	Scope        string        `json:"scopeDigest"`
	Thread       string        `json:"thread"`
	Registration string        `json:"registration,omitempty"`
	Epoch        string        `json:"epoch,omitempty"`
	Pending      *SteerAttempt `json:"pending,omitempty"`
}

func steerScope(scope string) string {
	v := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(v[:])
}
func emptySteerRecord(scope, thread string) SteerAttemptRecord {
	return SteerAttemptRecord{Format: 1, Scope: steerScope(scope), Thread: thread}
}

var steerID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var steerDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validSteerRecord(scope, thread string, r SteerAttemptRecord) bool {
	return r.Scope == steerScope(scope) && r.Thread == thread && validSteerStructure(r)
}

func validSteerStructure(r SteerAttemptRecord) bool {
	if r.Format != 1 || !steerDigest.MatchString(r.Scope) || !diagnosticSessionID.MatchString(r.Thread) {
		return false
	}
	if (r.Registration == "") != (r.Epoch == "") {
		return false
	}
	if r.Registration != "" && (!steerID.MatchString(r.Registration) || !steerID.MatchString(r.Epoch)) {
		return false
	}
	if r.Pending == nil {
		return true
	}
	a := r.Pending
	if r.Registration == "" || len(a.Sequences) == 0 || len(a.Sequences) > 32 || len(a.Turns) == 0 || len(a.Turns) > 2 {
		return false
	}
	var prior uint64
	for _, seq := range a.Sequences {
		if seq <= prior {
			return false
		}
		prior = seq
	}
	for _, turn := range a.Turns {
		if !steerID.MatchString(turn) {
			return false
		}
	}
	return a.ClientID == fmt.Sprintf("xb:%s:%s:%d-%d", r.Registration, r.Epoch, a.Sequences[0], prior)
}
func (s *Store) steerAttemptPath(scope, thread string) (string, error) {
	if !diagnosticSessionID.MatchString(thread) {
		return "", ErrSteerRecovery
	}
	for i, p := range []string{s.dir, filepath.Join(s.dir, "steer-attempts"), filepath.Join(s.dir, "steer-attempts", "v1")} {
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) && i > 0 {
			if err = os.Mkdir(p, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", ErrSteerRecovery
			}
			if err = protectSteerPath(p); err != nil {
				return "", ErrSteerRecovery
			}
			info, err = os.Lstat(p)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !privateSteerPath(p, info, i > 0) {
			return "", ErrSteerRecovery
		}
	}
	hash := sha256.Sum256([]byte(scope + "\x00" + thread))
	return filepath.Join(s.dir, "steer-attempts", "v1", hex.EncodeToString(hash[:])+".json"), nil
}
func readSteerAttempt(path, scope, thread string) (SteerAttemptRecord, error) {
	empty := emptySteerRecord(scope, thread)
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil || !before.Mode().IsRegular() || !privateSteerPath(path, before, true) {
		return empty, ErrSteerRecovery
	}
	f, err := os.Open(path)
	if err != nil {
		return empty, ErrSteerRecovery
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !os.SameFile(before, info) {
		return empty, ErrSteerRecovery
	}
	out, err := decodeSteerRecord(f)
	if err != nil || !validSteerRecord(scope, thread, out) {
		return empty, ErrSteerRecovery
	}
	return out, nil
}

// Shared bounded strict decode; callers separately bind raw scope and filename.
func decodeSteerRecord(reader io.Reader) (SteerAttemptRecord, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, steerAttemptLimit+1))
	if err != nil || len(raw) > steerAttemptLimit {
		return SteerAttemptRecord{}, ErrSteerRecovery
	}
	var out SteerAttemptRecord
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&out) != nil || d.Decode(new(any)) != io.EOF || !validSteerStructure(out) {
		return SteerAttemptRecord{}, ErrSteerRecovery
	}
	return out, nil
}

func (s *Store) lockSteerAttempt(scope, thread string) (string, func(), error) {
	path, err := s.steerAttemptPath(scope, thread)
	if err != nil {
		return "", nil, err
	}
	lock := path + ".lock"
	f, err := os.OpenFile(lock, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		f.Close()
		if protectSteerPath(lock) != nil {
			return "", nil, ErrSteerRecovery
		}
	} else if !errors.Is(err, os.ErrExist) {
		return "", nil, ErrSteerRecovery
	}
	before, err := os.Lstat(lock)
	if err != nil || !before.Mode().IsRegular() || !privateSteerPath(lock, before, true) {
		return "", nil, ErrSteerRecovery
	}
	if s.steerLockObserved != nil {
		s.steerLockObserved()
	}
	unlock, err := lockFileWait(lock, time.Second)
	if err != nil {
		return "", nil, ErrSteerRecovery
	}
	after, e := os.Lstat(lock)
	if e != nil || !os.SameFile(before, after) || !privateSteerPath(lock, after, true) {
		unlock()
		return "", nil, ErrSteerRecovery
	}
	return path, unlock, nil
}
func (s *Store) LoadSteerAttempt(scope, thread string) (SteerAttemptRecord, error) {
	path, unlock, err := s.lockSteerAttempt(scope, thread)
	if err != nil {
		return SteerAttemptRecord{}, err
	}
	defer unlock()
	return readSteerAttempt(path, scope, thread)
}

// Compare the whole body-free record: a retired writer cannot clear its successor.
func (s *Store) SaveSteerAttempt(scope, thread string, before, next SteerAttemptRecord) error {
	if !validSteerRecord(scope, thread, next) {
		return ErrSteerRecovery
	}
	path, unlock, err := s.lockSteerAttempt(scope, thread)
	if err != nil {
		return err
	}
	defer unlock()
	prior, err := readSteerAttempt(path, scope, thread)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(prior, before) {
		return ErrSteerChanged
	}
	raw, err := json.Marshal(next)
	if err != nil || len(raw) > steerAttemptLimit {
		return ErrSteerRecovery
	}
	if writeFile(path, raw) != nil || protectSteerPath(path) != nil {
		return ErrSteerRecovery
	}
	return nil
}
