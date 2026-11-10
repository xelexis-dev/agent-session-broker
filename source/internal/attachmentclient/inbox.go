package attachmentclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/pathguard"
	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

var idPattern = regexp.MustCompile(`^[A-Z2-7]{26}$`)
var keyPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func ValidID(id string) bool { return idPattern.MatchString(id) }

type Fetch func(context.Context) (io.ReadCloser, int64, string, string)

// Receive resolves transient downloads before exposing a sequence to ACK state.
func Receive(ctx context.Context, cache, key string, ref protocol.Attachment, fetch Fetch) *protocol.Attachment {
	return receive(ctx, cache, key, ref, fetch, func(dst io.Writer, src io.Reader) (int64, error) {
		return io.CopyBuffer(dst, src, make([]byte, 32<<10))
	})
}
func receive(ctx context.Context, cache, key string, ref protocol.Attachment, fetch Fetch, copyBytes func(io.Writer, io.Reader) (int64, error)) *protocol.Attachment {
	fail := func(code string) *protocol.Attachment { return &protocol.Attachment{Error: code} }
	if !ValidID(ref.ID) || !keyPattern.MatchString(key) || ref.Size < 1 || ref.Size > MaxFileBytes || len(ref.SHA256) != 64 {
		return fail("integrity")
	}
	hash, err := hex.DecodeString(ref.SHA256)
	if err != nil || len(hash) != 32 {
		return fail("integrity")
	}
	cache, err = cacheDirectory(cache)
	if err != nil {
		return fail("failed")
	}
	rootPath, _, root, err := pathguard.EnsureDirectory(filepath.Join(cache, "agent-session-broker", "inbox"), 0700, openPin, nil)
	if err != nil {
		return fail(storageCode(err))
	}
	defer root.Close()
	cleanParts(root, rootPath)
	canonical, _, pin, _, err := pathguard.EnsureChild(root, rootPath, key, true, 0700, openPin, nil)
	if err != nil {
		return fail(storageCode(err))
	}
	defer pin.Close()
	final := ref.ID + "-" + SanitizeName(ref.Name)
	projection := protocol.LocalAttachment(&protocol.Attachment{Path: filepath.Join(canonical, final), Size: ref.Size, SHA256: ref.SHA256})
	if projection.Error != "" {
		return projection
	}
	if exists, valid := existing(filepath.Join(pin.Path(), final), ref); exists {
		if valid {
			return projection
		}
		return fail("integrity")
	}
	budget, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	for {
		if budget.Err() != nil {
			return fail("failed")
		}
		body, size, digest, code := fetch(budget)
		if code == "" {
			if body == nil {
				return fail("failed")
			}
			code = saveAttempt(pin, ref, body, size, digest, copyBytes)
		}
		if body != nil {
			body.Close()
		}
		if code == "" {
			return projection
		}
		if code != "busy" && code != "retry" {
			switch code {
			case "expired", "unavailable", "integrity", "disk-full", "path-too-long":
				return fail(code)
			default:
				return fail("failed")
			}
		}
		select {
		case <-budget.Done():
			return fail("failed")
		case <-time.After(time.Second):
		}
	}
}

type downloadReader struct {
	io.Reader
	err error
}

func (r *downloadReader) Read(p []byte) (int, error) {
	n, e := r.Reader.Read(p)
	if e != nil && e != io.EOF {
		r.err = e
	}
	return n, e
}

func saveAttempt(pin pathguard.Pin, ref protocol.Attachment, body io.Reader, size int64, digest string, copyBytes func(io.Writer, io.Reader) (int64, error)) string {
	if size != ref.Size || digest != ref.SHA256 {
		return "integrity"
	}
	part := "." + ref.ID + "-" + rand.Text() + ".part"
	file, err := createPart(filepath.Join(pin.Path(), part))
	if err != nil {
		return storageCode(err)
	}
	closed := false
	defer func() {
		if !closed {
			file.Close()
		}
		_ = pfs.Remove(filepath.Join(pin.Path(), part))
	}()
	h := sha256.New()
	source := &downloadReader{Reader: body}
	n, err := copyBytes(io.MultiWriter(file, h), io.LimitReader(source, ref.Size+1))
	if source.err != nil {
		return "retry"
	}
	if err != nil {
		return storageCode(err)
	}
	if n < ref.Size {
		return "retry"
	}
	if n != ref.Size || hex.EncodeToString(h.Sum(nil)) != ref.SHA256 {
		return "integrity"
	}
	if err = file.Sync(); err != nil {
		return storageCode(err)
	}
	err = file.Close()
	closed = true
	if err != nil {
		return storageCode(err)
	}
	if pin.Validate() != nil {
		return "failed"
	}
	final := ref.ID + "-" + SanitizeName(ref.Name)
	if err = renameAbsent(filepath.Join(pin.Path(), part), filepath.Join(pin.Path(), final)); err != nil {
		if exists, valid := existing(filepath.Join(pin.Path(), final), ref); exists {
			if valid {
				return ""
			}
			return "integrity"
		}
		return storageCode(err)
	}
	return ""
}

func existing(path string, ref protocol.Attachment) (bool, bool) {
	info, err := pfs.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() != ref.Size {
		return true, false
	}
	file, err := openLeaf(path)
	if err != nil {
		return true, false
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return true, false
	}
	h := sha256.New()
	n, err := io.CopyBuffer(h, io.LimitReader(file, ref.Size+1), make([]byte, 32<<10))
	return true, err == nil && n == ref.Size && hex.EncodeToString(h.Sum(nil)) == ref.SHA256
}
func storageCode(err error) string {
	if diskFull(err) {
		return "disk-full"
	}
	return "failed"
}

// Cleanup owns only generated stale partial names; completed files never expire.
func cleanParts(root pathguard.Pin, canonical string) {
	lock, err := createLock(filepath.Join(root.Path(), ".cleanup.lock"))
	if err != nil {
		return
	}
	defer lock.Close()
	unlock, err := lockFile(lock)
	if err != nil {
		return
	}
	defer unlock()
	entries, err := pfs.ReadDir(root.Path())
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !keyPattern.MatchString(entry.Name()) {
			continue
		}
		_, _, pin, exists, err := pathguard.EnsureChild(root, canonical, entry.Name(), false, 0700, openPin, nil)
		if err != nil || !exists {
			continue
		}
		children, _ := pfs.ReadDir(pin.Path())
		for _, child := range children {
			name := child.Name()
			if !ownPartName(name) {
				continue
			}
			info, err := pfs.Lstat(filepath.Join(pin.Path(), name))
			if err == nil && info.Mode().IsRegular() && time.Since(info.ModTime()) > 30*time.Minute {
				_ = pfs.Remove(filepath.Join(pin.Path(), name))
			}
		}
		pin.Close()
	}
}

func cacheDirectory(cache string) (string, error) {
	absolute, err := filepath.Abs(cache)
	if err != nil {
		return "", err
	}
	existing := absolute
	var missing []string
	for {
		_, err = os.Lstat(existing)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", err
		}
		missing = append(missing, filepath.Base(existing))
		existing = parent
	}
	canonical, err := canonicalCWD(existing)
	if err != nil {
		return "", err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		canonical = filepath.Join(canonical, missing[i])
	}
	path, _, pin, err := pathguard.EnsureDirectory(canonical, 0700, openPin, nil)
	if err != nil {
		return "", err
	}
	defer pin.Close()
	return path, nil
}

func ownPartName(name string) bool {
	if !strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".part") {
		return false
	}
	ids := strings.Split(strings.TrimSuffix(name[1:], ".part"), "-")
	return len(ids) == 1 && ValidID(ids[0]) || len(ids) == 2 && ValidID(ids[0]) && ValidID(ids[1])
}
