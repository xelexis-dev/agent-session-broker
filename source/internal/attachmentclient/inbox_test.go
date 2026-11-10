package attachmentclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func reference(data []byte) protocol.Attachment {
	sum := sha256.Sum256(data)
	return protocol.Attachment{ID: strings.Repeat("A", 26), Name: "fixture.bin", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
}
func fixtureFetch(data []byte) Fetch {
	return func(context.Context) (io.ReadCloser, int64, string, string) {
		ref := reference(data)
		return io.NopCloser(bytes.NewReader(data)), ref.Size, ref.SHA256, ""
	}
}
func TestInboxIntegrityAndReuse(t *testing.T) {
	cache := realTemp(t)
	key := strings.Repeat("a", 32)
	data := []byte{0, 255, 91, 2, 44, 254}
	ref := reference(data)
	got := Receive(t.Context(), cache, key, ref, fixtureFetch(data))
	if got.Error != "" {
		t.Fatal(got.Error)
	}
	stored, e := os.ReadFile(got.Path)
	if e != nil || !bytes.Equal(stored, data) {
		t.Fatal("saved bytes differ")
	}
	calls := 0
	again := Receive(t.Context(), cache, key, ref, func(context.Context) (io.ReadCloser, int64, string, string) { calls++; return nil, 0, "", "failed" })
	if again.Path != got.Path || calls != 0 {
		t.Fatal("redelivery did not reuse verified file")
	}
	info, _ := os.Stat(got.Path)
	if info.Mode().Perm()&0111 != 0 {
		t.Fatal("received executable")
	}
	different := bytes.Repeat([]byte{42}, len(data))
	put(t, got.Path, different)
	mismatch := Receive(t.Context(), cache, key, ref, fixtureFetch(data))
	if mismatch.Error != "integrity" {
		t.Fatal("existing different file overwritten")
	}
	contents, _ := os.ReadFile(got.Path)
	if !bytes.Equal(contents, different) {
		t.Fatal("old file changed")
	}
}
func TestInboxMismatchAndErrors(t *testing.T) {
	for _, want := range []string{"integrity", "expired", "unavailable", "failed"} {
		t.Run(want, func(t *testing.T) {
			cache := realTemp(t)
			data := []byte("synthetic-binary")
			ref := reference(data)
			fetch := func(context.Context) (io.ReadCloser, int64, string, string) {
				if want == "integrity" {
					return io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{42}, len(data)))), ref.Size, ref.SHA256, ""
				}
				return nil, 0, "", want
			}
			got := Receive(t.Context(), cache, strings.Repeat("a", 32), ref, fetch)
			if got.Error != want {
				t.Fatalf("got %s", got.Error)
			}
			entries, _ := filepath.Glob(filepath.Join(cache, "agent-session-broker", "inbox", "*", "*.part"))
			if len(entries) != 0 {
				t.Fatal("partial leaked")
			}
		})
	}
}
func TestInboxBusyBudget(t *testing.T) {
	cache := realTemp(t)
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls := 0
		data := []byte("synthetic")
		got := Receive(t.Context(), cache, strings.Repeat("a", 32), reference(data), func(context.Context) (io.ReadCloser, int64, string, string) { calls++; return nil, 0, "", "busy" })
		if got.Error != "failed" || calls < 2 || time.Since(start) > 10*time.Minute {
			t.Fatal("busy budget differs")
		}
	})
}
func TestInboxBusyThenSuccess(t *testing.T) {
	cache := realTemp(t)
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		data := []byte("synthetic")
		got := Receive(t.Context(), cache, strings.Repeat("a", 32), reference(data), func(ctx context.Context) (io.ReadCloser, int64, string, string) {
			calls++
			if calls <= 3 {
				return nil, 0, "", "busy"
			}
			return fixtureFetch(data)(ctx)
		})
		if got.Error != "" || calls != 4 {
			t.Fatal("busy finalized early")
		}
	})
}

func TestInboxDiskFullAndPathBudget(t *testing.T) {
	cache := realTemp(t)
	data := []byte("synthetic")
	key := strings.Repeat("a", 32)
	got := receive(t.Context(), cache, key, reference(data), fixtureFetch(data), func(io.Writer, io.Reader) (int64, error) { return 0, fullDiskError() })
	if got.Error != "disk-full" {
		t.Fatal("disk-full injection not mapped")
	}
	long := filepath.Join(cache, strings.Repeat("&", 150), strings.Repeat("&", 150), strings.Repeat("&", 100))
	if e := os.MkdirAll(long, 0700); e != nil {
		t.Fatal(e)
	}
	got = Receive(t.Context(), long, key, reference(data), func(context.Context) (io.ReadCloser, int64, string, string) {
		t.Fatal("path overflow fetched bytes")
		return nil, 0, "", "failed"
	})
	if got.Error != "path-too-long" {
		t.Fatal("HTML-escaped path budget not enforced")
	}
}
func TestInboxStaleOwnPartsOnly(t *testing.T) {
	cache := realTemp(t)
	key := strings.Repeat("a", 32)
	data := []byte("synthetic")
	got := Receive(t.Context(), cache, key, reference(data), fixtureFetch(data))
	if got.Error != "" {
		t.Fatal(got.Error)
	}
	dir := filepath.Dir(got.Path)
	old := filepath.Join(dir, "."+strings.Repeat("B", 26)+".part")
	fresh := filepath.Join(dir, "."+strings.Repeat("C", 26)+".part")
	foreign := filepath.Join(dir, ".foreign.part")
	for _, p := range []string{old, fresh, foreign} {
		put(t, p, []byte("synthetic"))
	}
	for _, p := range []string{old, foreign} {
		past := time.Now().Add(-31 * time.Minute)
		if e := os.Chtimes(p, past, past); e != nil {
			t.Fatal(e)
		}
	}
	got = Receive(t.Context(), cache, key, reference(data), fixtureFetch(data))
	if got.Error != "" {
		t.Fatal(got.Error)
	}
	if _, e := os.Stat(old); !os.IsNotExist(e) {
		t.Fatal("own stale partial retained")
	}
	for _, p := range []string{fresh, foreign, got.Path} {
		if _, e := os.Stat(p); e != nil {
			t.Fatal("unowned/fresh/completed entry removed")
		}
	}
}

func TestInboxDestinationRace(t *testing.T) {
	cache := realTemp(t)
	key := strings.Repeat("a", 32)
	data := []byte("synthetic")
	ref := reference(data)
	final := filepath.Join(cache, "agent-session-broker", "inbox", key, ref.ID+"-"+ref.Name)
	changed := bytes.Repeat([]byte{42}, len(data))
	got := Receive(t.Context(), cache, key, ref, func(ctx context.Context) (io.ReadCloser, int64, string, string) {
		put(t, final, changed)
		return fixtureFetch(data)(ctx)
	})
	if got.Error != "integrity" {
		t.Fatal("destination race overwrote existing file")
	}
	saved, e := os.ReadFile(final)
	if e != nil || !bytes.Equal(saved, changed) {
		t.Fatal("raced destination changed")
	}
}

func TestInboxCreatesCacheDirectory(t *testing.T) {
	cache := filepath.Join(realTemp(t), "new-cache")
	data := []byte("synthetic")
	got := Receive(t.Context(), cache, strings.Repeat("a", 32), reference(data), fixtureFetch(data))
	if got.Error != "" {
		t.Fatal("fresh OS cache could not be created")
	}
}

func TestInboxTransientHeaderAndBody(t *testing.T) {
	for _, stage := range []string{"header", "body"} {
		t.Run(stage, func(t *testing.T) {
			cache := realTemp(t)
			data := []byte("synthetic")
			ref := reference(data)
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				got := Receive(t.Context(), cache, strings.Repeat("a", 32), ref, func(ctx context.Context) (io.ReadCloser, int64, string, string) {
					calls++
					if calls == 1 {
						if stage == "header" {
							return nil, 0, "", "retry"
						}
						return io.NopCloser(&truncatedAttachmentReader{}), ref.Size, ref.SHA256, ""
					}
					return fixtureFetch(data)(ctx)
				})
				if calls != 2 || got.Error != "" {
					t.Fatal("transient download finalized before successful retry")
				}
			})
		})
	}
}

type truncatedAttachmentReader struct{}

func (*truncatedAttachmentReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestInboxFreshCrashPartial(t *testing.T) {
	cache := realTemp(t)
	key := strings.Repeat("a", 32)
	data := []byte("synthetic")
	ref := reference(data)
	partial := filepath.Join(cache, "agent-session-broker", "inbox", key, "."+ref.ID+".part")
	old := []byte("unfinished-other-attempt")
	put(t, partial, old)
	got := Receive(t.Context(), cache, key, ref, fixtureFetch(data))
	if got.Error != "" {
		t.Fatal("fresh crash partial blocked redelivery")
	}
	stored, e := os.ReadFile(got.Path)
	if e != nil || !bytes.Equal(stored, data) {
		t.Fatal("crash recovery bytes differ")
	}
	retained, e := os.ReadFile(partial)
	if e != nil || !bytes.Equal(retained, old) {
		t.Fatal("potentially active foreign partial changed")
	}
}

func TestInboxTransientBudget(t *testing.T) {
	cache := realTemp(t)
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls := 0
		data := []byte("synthetic")
		got := Receive(t.Context(), cache, strings.Repeat("a", 32), reference(data), func(context.Context) (io.ReadCloser, int64, string, string) { calls++; return nil, 0, "", "retry" })
		if got.Error != "failed" || calls < 2 || time.Since(start) != 10*time.Minute {
			t.Fatal("transient GET budget is not bounded to ten minutes")
		}
	})
}
