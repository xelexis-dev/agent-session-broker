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

func TestAttachmentDiskFloor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		capacity Capacity
		err      error
		accept   bool
	}{
		{"below", Capacity{Free: DiskFloorBytes, Total: 2 << 30}, nil, false},
		{"boundary", Capacity{Free: DiskFloorBytes + 1, Total: 2 << 30}, nil, true},
		{"above", Capacity{Free: DiskFloorBytes + 2, Total: 2 << 30}, nil, true},
		{"empty", Capacity{}, nil, false},
		{"invalid", Capacity{Free: 3 << 30, Total: 2 << 30}, nil, false},
		{"failure", Capacity{Free: 2 << 30, Total: 3 << 30}, errors.New("synthetic capacity failure"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := fixture(t)
			s.capacity = func(file *os.File) (Capacity, error) {
				if file == nil {
					t.Fatal("wrong volume")
				}
				return tc.capacity, tc.err
			}
			var body io.Reader = forbiddenReader{}
			if tc.accept {
				body = strings.NewReader("x")
			}
			_, err := s.Upload(t.Context(), owner, "f", 1, body)
			if tc.accept {
				if err != nil {
					t.Fatal("floor refused boundary", err)
				}
			} else {
				code(t, err, 507)
				if err.Error() != "server-disk-low" {
					t.Fatal("disk code")
				}
			}
		})
	}
}

func TestAttachmentDiskLowKeepsDownloadAndBinding(t *testing.T) {
	s, _ := fixture(t)
	id := upload(t, s)
	s.capacity = func(*os.File) (Capacity, error) { return Capacity{Free: 1, Total: 2 << 30}, nil }
	if _, err := s.Bind(owner, id, "receiver", 1); err != nil {
		t.Fatal("low disk blocked binding")
	}
	d, err := s.Get("project", "receiver", id)
	if err != nil {
		t.Fatal("low disk blocked download")
	}
	defer d.Close()
	b, err := io.ReadAll(d)
	if err != nil || string(b) != "blob" {
		t.Fatal("low disk download bytes")
	}
	capacity, err := s.DiskCapacity()
	if err != nil || capacity.Free != 1 || capacity.Total != 2<<30 {
		t.Fatal("capacity not exposed")
	}
}

func TestAttachmentExpiryStartsAtUpload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := fixture(t)
		id := upload(t, s)
		time.Sleep(9 * time.Minute)
		if _, err := s.Bind(owner, id, "receiver", 1); err != nil {
			t.Fatal("late bind")
		}
		time.Sleep(20*time.Minute + 59*time.Second)
		d, err := s.Get("project", "receiver", id)
		if err != nil {
			t.Fatal("expired early")
		}
		d.Close()
		time.Sleep(time.Second)
		synctest.Wait()
		d, err = s.Get("project", "receiver", id)
		if d != nil {
			d.Close()
			t.Fatal("binding extended expiry")
		}
		var e *Error
		if !errors.As(err, &e) || (e.Status != 404 && e.Status != 410) {
			t.Fatal("expiry status")
		}
	})
}

func TestSpoolVolumeCapacity(t *testing.T) {
	s, dir := fixture(t)
	s.capacity = diskCapacity
	file, err := os.Open(filepath.Join(dir, "attachments-v1"))
	if err != nil {
		t.Fatal("spool directory")
	}
	defer file.Close()
	expected, err := diskCapacity(file)
	if err != nil {
		t.Fatal("spool capacity")
	}
	capacity, err := s.DiskCapacity()
	if capacity.Total != expected.Total {
		t.Fatal("capacity measured another volume")
	}
	if err != nil || capacity.Total == 0 || capacity.Free > capacity.Total {
		t.Fatal("native capacity unavailable")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func TestAttachmentAccepts100MiB(t *testing.T) {
	s, _ := fixture(t)
	ref, err := s.Upload(t.Context(), owner, "f", 100<<20, io.LimitReader(zeroReader{}, 100<<20))
	if err != nil || ref.Size != 100<<20 {
		t.Fatal("100MiB boundary refused", err)
	}
}

type partialBarrierReader struct {
	entered, release chan struct{}
	reads            int
}

func (r *partialBarrierReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads > 2 {
		return 0, io.EOF
	}
	if r.reads == 2 {
		close(r.entered)
		<-r.release
	}
	p[0] = 'x'
	return 1, nil
}

type observedReader struct{ read bool }

func (r *observedReader) Read([]byte) (int, error) { r.read = true; return 0, io.EOF }
func TestAttachmentConcurrentDiskReservation(t *testing.T) {
	s, _ := fixture(t)
	s.capacity = func(*os.File) (Capacity, error) { return Capacity{Free: DiskFloorBytes + 2, Total: 2 << 30}, nil }
	first := &partialBarrierReader{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := s.Upload(t.Context(), owner, "f", 2, first); done <- err }()
	<-first.entered
	other := owner
	other.Registration = "other"
	body := &observedReader{}
	_, err := s.Upload(t.Context(), other, "f", 1, body)
	close(first.release)
	firstErr := <-done
	code(t, err, 507)
	if body.read {
		t.Fatal("reserved admission read rejected body")
	}
	if firstErr != nil {
		t.Fatal("first upload")
	}
	if _, err = s.Upload(t.Context(), other, "f", 1, strings.NewReader("x")); err != nil {
		t.Fatal("completed reservation retained")
	}
}

func TestAttachmentDiskReservationReleaseOnExit(t *testing.T) {
	for _, kind := range []string{"success", "error", "cancel", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, _ := fixture(t)
				s.capacity = func(*os.File) (Capacity, error) { return Capacity{Free: DiskFloorBytes + 1, Total: 2 << 30}, nil }
				if kind == "success" || kind == "error" {
					body := "x"
					if kind == "error" {
						body = "xx"
					}
					_, err := s.Upload(t.Context(), owner, "f", 1, strings.NewReader(body))
					if (err == nil) != (kind == "success") {
						t.Fatal("upload result")
					}
				} else {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					reader, writer := io.Pipe()
					defer writer.Close()
					done := make(chan error, 1)
					go func() { _, err := s.Upload(ctx, owner, "f", 1, reader); done <- err }()
					synctest.Wait()
					if kind == "cancel" {
						cancel()
					} else {
						time.Sleep(10 * time.Minute)
					}
					synctest.Wait()
					if <-done == nil {
						t.Fatal("canceled/timed upload accepted")
					}
				}
				if s.reserved != 0 {
					t.Fatal("reservation leaked on " + kind)
				}
				other := owner
				other.Registration = "after"
				if _, err := s.Upload(t.Context(), other, "f", 1, strings.NewReader("x")); err != nil {
					t.Fatal("released capacity unavailable")
				}
			})
		})
	}
}
