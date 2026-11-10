package memory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestPersistedReadSizeHint(t *testing.T) {
	for _, tc := range []struct {
		name           string
		statSize, size int
		oversize       bool
	}{
		{"empty", 0, 0, false}, {"exactStat", 17, 17, false}, {"max", maxPersistedNodeBytes, maxPersistedNodeBytes, false},
		{"shrink", 64, 2, false}, {"grow", 2, 64, false}, {"oversizedGrowth", 2, maxPersistedNodeBytes + 100, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, e := os.OpenFile(filepath.Join(t.TempDir(), "node"), os.O_CREATE|os.O_RDWR, 0600)
			if e != nil {
				t.Fatal(e)
			}
			defer file.Close()
			if e = file.Truncate(int64(tc.statSize)); e != nil {
				t.Fatal(e)
			}
			info, e := file.Stat()
			if e != nil {
				t.Fatal(e)
			}
			// Mutate the same opened inode after its pinned Stat, before reading.
			payload := bytes.Repeat([]byte("x"), tc.size)
			if e = file.Truncate(int64(tc.size)); e != nil {
				t.Fatal(e)
			}
			if _, e = file.WriteAt(payload, 0); e != nil {
				t.Fatal(e)
			}
			got, e := readPersistedNode(t.Context(), file, info.Size())
			if tc.oversize {
				if !errors.Is(e, ErrInvalidInput) {
					t.Fatalf("oversized growth: %v", e)
				}
				offset, _ := file.Seek(0, io.SeekCurrent)
				if offset > maxPersistedNodeBytes+1 {
					t.Fatal("read exceeded overflow probe")
				}
				return
			}
			if e != nil || !bytes.Equal(got, payload) {
				t.Fatalf("hint truncated or rejected valid data: len=%d err=%v", len(got), e)
			}
		})
	}
}

type persistedReaderFunc func([]byte) (int, error)

func (f persistedReaderFunc) Read(p []byte) (int, error) { return f(p) }
func TestPersistedReadShortAndEOF(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), maxPersistedNodeBytes)
	for _, hint := range []int64{-1, 0, 17, int64(maxPersistedNodeBytes), math.MaxInt64} {
		for _, withEOF := range []bool{false, true} {
			offset := 0
			reader := persistedReaderFunc(func(p []byte) (int, error) {
				if len(p) == 0 {
					t.Fatal("zero-length read")
				}
				if offset == len(payload) {
					return 0, io.EOF
				}
				n := min(len(p), 17, len(payload)-offset)
				copy(p, payload[offset:offset+n])
				offset += n
				if withEOF && offset == len(payload) {
					return n, io.EOF
				}
				return n, nil
			})
			got, e := readPersistedNode(t.Context(), reader, hint)
			if e != nil || !bytes.Equal(got, payload) {
				t.Fatalf("short read/EOF hint%d: %v", hint, e)
			}
		}
	}
}
func TestPersistedReadErrorOrdering(t *testing.T) {
	sentinel := errors.New("synthetic read failure")
	for _, tc := range []struct {
		name    string
		n       int
		readErr error
		cancel  bool
		want    error
	}{
		{"noProgress", 0, nil, false, io.ErrNoProgress}, {"readerError", 1, sentinel, false, sentinel},
		{"cancelOverridesEOF", 1, io.EOF, true, context.Canceled}, {"cancelOverridesError", 1, sentinel, true, context.Canceled},
		{"overflowOverridesCancel", maxPersistedNodeBytes + 1, io.EOF, true, ErrInvalidInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			reader := persistedReaderFunc(func(p []byte) (int, error) {
				calls++
				if len(p) < tc.n {
					t.Fatal("bounded destination too small")
				}
				if tc.cancel {
					cancel()
				}
				return tc.n, tc.readErr
			})
			got, e := readPersistedNode(ctx, reader, maxPersistedNodeBytes)
			if !errors.Is(e, tc.want) || got != nil || calls != 1 {
				t.Fatalf("read ordering: %v, calls=%d", e, calls)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, e := readPersistedNode(ctx, persistedReaderFunc(func([]byte) (int, error) { t.Fatal("read after cancellation"); return 0, nil }), 0)
	if !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
