// Package attachments owns the ephemeral server attachment spool.
package attachments

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/pathguard"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
	"github.com/xelexis-dev/agent-session-broker/source/internal/serverdiag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

const MaxFileBytes int64 = 100 << 20
const DiskFloorBytes uint64 = 1 << 30
const MaxTransfers = 8
const Expiry = 30 * time.Minute

var idPattern = regexp.MustCompile(`^[A-Z2-7]{26}$`)
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,63}$`)

type Owner struct{ Project, Registration, Token string }
type Error struct {
	Status int
	Code   string
}

func (e *Error) Error() string              { return e.Code }
func problem(status int, code string) error { return &Error{status, code} }

type Download struct {
	io.ReadCloser
	Ref protocol.Attachment
}
type record struct {
	reason          string
	owner           Owner
	ref             protocol.Attachment
	recipient       string
	sequence        uint64
	created         time.Time
	active, deleted bool
	cancel          context.CancelFunc
	readers         map[*os.File]bool
}
type Store struct {
	pending         map[[2]string]bool
	mu              sync.Mutex
	root            *os.Root
	pin             pathguard.Pin
	items           map[string]*record
	capacity        func(*os.File) (Capacity, error)
	active          int
	reserved        int64
	enabled, closed bool
	stop, done      chan struct{}
	once            sync.Once
}

func Open(directory string) (*Store, error) {
	dir, err := filepath.Abs(filepath.Join(directory, "attachments-v1"))
	if err != nil {
		return nil, problem(500, "unavailable")
	}
	_, _, pin, err := pathguard.EnsureDirectory(dir, 0700, func(a, b string, i os.FileInfo) (pathguard.Pin, error) {
		return pathguard.NewPin(a, b, i, pathguard.PinLabels{Store: "attachment", Access: "attachment", Invalid: errors.New("unavailable")})
	}, nil)
	if err != nil {
		return nil, problem(500, "unavailable")
	}
	root, err := os.OpenRoot(pin.Path())
	if err != nil {
		pin.Close()
		return nil, problem(500, "unavailable")
	}
	fail := func() (*Store, error) { root.Close(); pin.Close(); return nil, problem(500, "unavailable") }
	f, err := root.Open(".")
	if err != nil {
		return fail()
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return fail()
	}
	for _, name := range names {
		id := name
		if len(name) > 5 && name[len(name)-5:] == ".part" {
			id = name[:len(name)-5]
		}
		if idPattern.MatchString(id) {
			if root.Remove(name) != nil {
				return fail()
			}
			serverdiag.Emit(serverdiag.Event{Type: "attachment-removed", Reason: "startup"})
		}
	}
	s := &Store{capacity: diskCapacity, root: root, pin: pin, items: map[string]*record{}, pending: map[[2]string]bool{}, stop: make(chan struct{}), done: make(chan struct{})}
	go s.sweep()
	return s, nil
}
func (s *Store) sweep() {
	defer close(s.done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-ticker.C:
			s.mu.Lock()
			for _, r := range s.items {
				ttl := Expiry
				if r.recipient == "" {
					ttl = 10 * time.Minute
				}
				if r.deleted || !now.Before(r.created.Add(ttl)) {
					s.deleteLocked(r, "expired")
				}
			}
			s.mu.Unlock()
		}
	}
}
func (s *Store) Close() error {
	var err error
	s.once.Do(func() {
		close(s.stop)
		<-s.done
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
		s.enabled = false
		for _, r := range s.items {
			s.deleteLocked(r, "discard")
		}
		err = errors.Join(s.root.Close(), s.pin.Close())
	})
	return err
}
func (s *Store) Enabled() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.enabled && !s.closed }
func (s *Store) SetEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = enabled && !s.closed
	if !s.enabled {
		for _, r := range s.items {
			s.deleteLocked(r, "disabled")
		}
	}
}
func (s *Store) deleteLocked(r *record, reason string) {
	if !r.deleted {
		r.reason = reason
	}
	r.deleted = true
	if r.cancel != nil {
		r.cancel()
	}
	for f := range r.readers {
		_ = f.Close()
	}
	name := r.ref.ID
	if r.active {
		name += ".part"
	}
	err := s.root.Remove(name)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		if s.items[r.ref.ID] == r {
			delete(s.items, r.ref.ID)
			serverdiag.Emit(serverdiag.Event{Type: "attachment-removed", Reason: r.reason})
		}
	}
}
func (s *Store) Delete(id, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.items[id]; r != nil {
		s.deleteLocked(r, reason)
	}
}
func (s *Store) DeleteOwned(owner Owner, id string, bindingSequence uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.items[id]; r != nil && r.owner == owner && (r.recipient == "" || bindingSequence != 0 && r.sequence == bindingSequence) {
		s.deleteLocked(r, "send-failed")
	}
}
func (s *Store) Upload(ctx context.Context, owner Owner, name string, size int64, body io.Reader) (protocol.Attachment, error) {
	fail := func(status int, code string) (protocol.Attachment, error) {
		return protocol.Attachment{}, problem(status, code)
	}
	s.mu.Lock()
	if !s.enabled || s.closed {
		s.mu.Unlock()
		return fail(403, "attachments-disabled")
	}
	if owner.Project == "" || owner.Registration == "" || owner.Token == "" || !namePattern.MatchString(name) {
		s.mu.Unlock()
		return fail(400, "invalid")
	}
	if size < 1 || size > MaxFileBytes {
		s.mu.Unlock()
		return fail(413, "too-large")
	}
	if s.pending[[2]string{owner.Project, owner.Registration}] {
		s.mu.Unlock()
		return fail(409, "busy")
	}
	for _, r := range s.items {
		if r.owner.Project == owner.Project && r.owner.Registration == owner.Registration && r.recipient == "" && !r.deleted {
			if r.active {
				s.mu.Unlock()
				return fail(409, "busy")
			}
			s.deleteLocked(r, "send-failed")
		}
	}
	if s.active >= MaxTransfers {
		s.mu.Unlock()
		return fail(503, "busy")
	}
	capacity, err := s.diskCapacityLocked()
	if err != nil || capacity.Free < DiskFloorBytes+uint64(s.reserved)+uint64(size) {
		s.mu.Unlock()
		return fail(507, "server-disk-low")
	}
	id := rand.Text()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	r := &record{owner: owner, ref: protocol.Attachment{ID: id, Name: name, Size: size}, created: time.Now(), active: true, cancel: cancel, readers: map[*os.File]bool{}}
	s.items[id] = r
	s.pending[[2]string{owner.Project, owner.Registration}] = true
	s.active++
	// Keep the full reservation until exit: free space may already include partial writes.
	s.reserved += size
	s.mu.Unlock()
	if closer, ok := body.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	defer cancel()
	success := false
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.active--
		s.reserved -= size
		delete(s.pending, [2]string{owner.Project, owner.Registration})
		if !success {
			s.deleteLocked(r, "send-failed")
		}
	}()
	f, err := s.root.OpenFile(id+".part", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fail(500, "unavailable")
	}
	hash := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(f, hash), io.LimitReader(body, size), make([]byte, 32<<10))
	closeErr := f.Close()
	var extra [1]byte
	extraBytes, extraErr := io.ReadFull(body, extra[:])
	if extraBytes != 0 || (extraErr != nil && !errors.Is(extraErr, io.EOF)) {
		return fail(413, "too-large")
	}
	if ctx.Err() != nil {
		return fail(403, "unavailable")
	}
	if n != size {
		return fail(413, "too-large")
	}
	if err != nil || closeErr != nil {
		return fail(500, "unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || s.closed || r.deleted || s.items[id] != r {
		return fail(403, "attachments-disabled")
	}
	if s.pin.Validate() != nil || s.root.Rename(id+".part", id) != nil {
		return fail(500, "unavailable")
	}
	r.active = false
	r.created = time.Now()
	serverdiag.Emit(serverdiag.Event{Type: "attachment-stored", Bytes: size})
	r.ref.SHA256 = hex.EncodeToString(hash.Sum(nil))
	success = true
	return r.ref, nil
}
func (s *Store) Bind(owner Owner, id, recipient string, sequence uint64) (*protocol.Attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || s.closed {
		return nil, problem(403, "attachments-disabled")
	}
	r := s.items[id]
	if r == nil || r.deleted {
		return nil, problem(404, "unavailable")
	}
	if r.owner != owner {
		return nil, problem(403, "forbidden")
	}
	if r.active || r.recipient != "" || recipient == "" || sequence == 0 {
		return nil, problem(409, "invalid")
	}
	if !time.Now().Before(r.created.Add(10 * time.Minute)) {
		s.deleteLocked(r, "expired")
		return nil, problem(404, "unavailable")
	}
	r.recipient = recipient
	r.sequence = sequence
	ref := r.ref
	return &ref, nil
}

type downloadReader struct {
	file   *os.File
	store  *Store
	record *record
	once   sync.Once
}

func (d *downloadReader) Read(p []byte) (int, error) { return d.file.Read(p) }
func (d *downloadReader) Close() error {
	var err error
	d.once.Do(func() {
		err = d.file.Close()
		s := d.store
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(d.record.readers, d.file)
		s.active--
		if d.record.deleted {
			s.deleteLocked(d.record, d.record.reason)
		}
	})
	return err
}
func (s *Store) Get(project, recipient, id string) (*Download, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || s.closed {
		return nil, problem(403, "attachments-disabled")
	}
	r := s.items[id]
	if r == nil || r.deleted || r.active || r.owner.Project != project || r.recipient != recipient || r.recipient == "" {
		return nil, problem(404, "unavailable")
	}
	if !time.Now().Before(r.created.Add(Expiry)) {
		s.deleteLocked(r, "expired")
		return nil, problem(410, "expired")
	}
	if s.active >= MaxTransfers {
		return nil, problem(503, "busy")
	}
	if s.pin.Validate() != nil {
		return nil, problem(500, "unavailable")
	}
	f, err := s.root.Open(id)
	if err != nil {
		return nil, problem(404, "unavailable")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != r.ref.Size {
		f.Close()
		return nil, problem(404, "unavailable")
	}
	s.active++
	r.readers[f] = true
	return &Download{ReadCloser: &downloadReader{file: f, store: s, record: r}, Ref: r.ref}, nil
}
