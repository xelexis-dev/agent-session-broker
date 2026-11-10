package attachments

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

var owner = Owner{"project", "sender", "token"}

func fixture(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal("open")
	}
	t.Cleanup(func() { s.Close() })
	s.capacity = func(*os.File) (Capacity, error) { return Capacity{Free: 10 << 30, Total: 20 << 30}, nil }
	s.SetEnabled(true)
	return s, dir
}
func upload(t *testing.T, s *Store) string {
	t.Helper()
	ref, e := s.Upload(t.Context(), owner, "fixture.bin", 4, strings.NewReader("blob"))
	if e != nil {
		t.Fatal("upload", e)
	}
	return ref.ID
}
func code(t *testing.T, err error, status int) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Status != status {
		t.Fatalf("want status%d, got %v", status, err)
	}
}
func TestAttachmentOwnerAndRecipient(t *testing.T) {
	for _, kind := range []string{"owner", "token", "project", "download"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := fixture(t)
			id := upload(t, s)
			wrong := owner
			switch kind {
			case "owner":
				wrong.Registration = "other"
			case "token":
				wrong.Token = "other"
			case "project":
				wrong.Project = "other"
			}
			if kind != "download" {
				_, e := s.Bind(wrong, id, "receiver", 1)
				code(t, e, 403)
				return
			}
			if _, e := s.Bind(owner, id, "receiver", 1); e != nil {
				t.Fatal("bind")
			}
			d, e := s.Get("project", "other", id)
			if d != nil {
				d.Close()
			}
			code(t, e, 404)
			d, e = s.Get("other", "receiver", id)
			if d != nil {
				d.Close()
			}
			code(t, e, 404)
		})
	}
}
func TestAttachmentDisabledAndDeletion(t *testing.T) {
	s, dir := fixture(t)
	id := upload(t, s)
	s.SetEnabled(false)
	_, e := s.Upload(t.Context(), owner, "f", 1, strings.NewReader("x"))
	code(t, e, 403)
	d, e := s.Get("project", "receiver", id)
	if d != nil {
		d.Close()
	}
	code(t, e, 403)
	if _, e = os.Stat(filepath.Join(dir, "attachments-v1", id)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("disabled bytes retained")
	}
}
func TestAttachmentSizeQuotaAndReplacement(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
		body string
	}{{"missing", 0, "x"}, {"huge", MaxFileBytes + 1, "x"}, {"short", 2, "x"}, {"overflow", 1, "xx"}} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := fixture(t)
			_, e := s.Upload(t.Context(), owner, "f", tc.size, strings.NewReader(tc.body))
			code(t, e, 413)
		})
	}
	t.Run("files", func(t *testing.T) {
		s, _ := fixture(t)
		for i := 0; i < 257; i++ {
			o := owner
			o.Registration = string(rune(i + 1))
			ref, e := s.Upload(t.Context(), o, "f", 1, strings.NewReader("x"))
			if e != nil {
				t.Fatal("removed global file quota still enforced")
			}
			if _, e = s.Bind(o, ref.ID, "receiver", uint64(i+1)); e != nil {
				t.Fatal("bind")
			}
		}
		_, e := s.Upload(t.Context(), owner, "f", 1, strings.NewReader("x"))
		if e != nil {
			t.Fatal("unlimited spool upload", e)
		}
	})
	t.Run("replacement", func(t *testing.T) {
		s, dir := fixture(t)
		a := upload(t, s)
		b := upload(t, s)
		if a == b {
			t.Fatal("upload was not replaced")
		}
		if _, e := os.Stat(filepath.Join(dir, "attachments-v1", a)); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("replaced bytes retained")
		}
	})
}

type barrierReader struct {
	entered chan struct{}
	release chan struct{}
}

func (r barrierReader) Read(p []byte) (int, error) {
	select {
	case <-r.entered:
		return 0, io.EOF
	default:
	}
	close(r.entered)
	<-r.release
	copy(p, "x")
	return 1, io.EOF
}
func TestAttachmentActiveAndTransferLimits(t *testing.T) {
	s, _ := fixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, e := s.Upload(context.Background(), owner, "f", 1, barrierReader{entered, release})
		done <- e
	}()
	<-entered
	_, e := s.Upload(t.Context(), owner, "f", 1, strings.NewReader("x"))
	close(release)
	<-done
	code(t, e, 409)
	id := upload(t, s)
	if _, e = s.Bind(owner, id, "receiver", 1); e != nil {
		t.Fatal("bind")
	}
	var held []*Download
	defer func() {
		for _, d := range held {
			d.Close()
		}
	}()
	for i := 0; i < MaxTransfers; i++ {
		d, e := s.Get("project", "receiver", id)
		if e != nil {
			t.Fatal("transfer early")
		}
		held = append(held, d)
	}
	d, e := s.Get("project", "receiver", id)
	if d != nil {
		d.Close()
	}
	code(t, e, 503)
	_, e = s.Upload(t.Context(), owner, "f", 1, forbiddenReader{})
	code(t, e, 503)
}
func TestAttachmentStartWipe(t *testing.T) {
	s, dir := fixture(t)
	id := upload(t, s)
	s.Close()
	// Simulate crash leftovers; a graceful Close already removed the live object.
	for _, name := range []string{id, id + ".part"} {
		if err := os.WriteFile(filepath.Join(dir, "attachments-v1", name), []byte("orphan"), 0600); err != nil {
			t.Fatal("orphan fixture")
		}
	}
	next, e := Open(dir)
	if e != nil {
		t.Fatal("reopen")
	}
	defer next.Close()
	if _, e = os.Stat(filepath.Join(dir, "attachments-v1", id)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("start retained bytes")
	}
	if _, e = os.Stat(filepath.Join(dir, "attachments-v1", id+".part")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("start retained part")
	}
}
func TestAttachmentTimers(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound", true: "bound"}[bound], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, _ := fixture(t)
				id := upload(t, s)
				if bound {
					if _, e := s.Bind(owner, id, "receiver", 1); e != nil {
						t.Fatal("bind")
					}
					time.Sleep(30 * time.Minute)
				} else {
					time.Sleep(11 * time.Minute)
				}
				synctest.Wait()
				if _, e := os.Stat(filepath.Join(s.root.Name(), id)); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("sweeper retained expired bytes")
				}
				_, e := s.Bind(owner, id, "receiver", 2)
				code(t, e, 404)
			})
		})
	}
}

type forbiddenReader struct{}

func (forbiddenReader) Read([]byte) (int, error) { panic("disabled request read bytes") }
func TestAttachmentDisabledBeforeRead(t *testing.T) {
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal("open")
	}
	defer s.Close()
	_, e = s.Upload(t.Context(), owner, "f", 1, forbiddenReader{})
	code(t, e, 403)
}
func TestAttachmentRequestDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := fixture(t)
		r, w := io.Pipe()
		defer w.Close()
		done := make(chan error, 1)
		go func() { _, e := s.Upload(t.Context(), owner, "f", 1, r); done <- e }()
		synctest.Wait()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if e := <-done; e == nil {
			t.Fatal("upload deadline ignored")
		}
	})
}

func TestAttachmentSizeBeforeRead(t *testing.T) {
	for _, size := range []int64{0, MaxFileBytes + 1} {
		s, _ := fixture(t)
		_, e := s.Upload(t.Context(), owner, "f", size, forbiddenReader{})
		code(t, e, 413)
	}
}

type heldUpload struct{ barrierReader }

func (heldUpload) Close() error { return nil }
func TestAttachmentActiveSurvivesDisableReenable(t *testing.T) {
	s, _ := fixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, e := s.Upload(t.Context(), owner, "f", 1, heldUpload{barrierReader{entered, release}})
		done <- e
	}()
	<-entered
	s.SetEnabled(false)
	s.SetEnabled(true)
	_, e := s.Upload(t.Context(), owner, "f", 1, strings.NewReader("x"))
	close(release)
	<-done
	code(t, e, 409)
}

func TestAttachmentDownloadExistencePrivacy(t *testing.T) {
	s, _ := fixture(t)
	id := upload(t, s)
	if _, err := s.Bind(owner, id, "receiver", 1); err != nil {
		t.Fatal("bind")
	}
	for _, expired := range []bool{false, true} {
		if expired {
			s.items[id].created = time.Now().Add(-31 * time.Minute)
		}
		for _, requester := range [][2]string{{"project", "other"}, {"other", "receiver"}, {"project", "sender"}} {
			for _, candidate := range []string{id, strings.Repeat("A", 26)} {
				d, err := s.Get(requester[0], requester[1], candidate)
				if d != nil {
					d.Close()
					t.Fatal("unauthorized bytes")
				}
				code(t, err, 404)
				if err.Error() != "unavailable" {
					t.Fatal("existence disclosed")
				}
			}
		}
	}
	_, err := s.Get("project", "receiver", id)
	code(t, err, 410)
}

func TestAttachment100MiBLimit(t *testing.T) {
	if MaxFileBytes != 100<<20 {
		t.Fatal("per-file limit is not 100MiB")
	}
}
