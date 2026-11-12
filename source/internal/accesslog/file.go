package accesslog

import (
	"container/heap"
	"crypto/rand"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/pathguard"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

type logHandle interface {
	io.Writer
	Stat() (os.FileInfo, error)
	Sync() error
	Close() error
}
type fileWriter struct {
	root                          *os.Root
	dir                           *os.File
	pin                           pathguard.Pin
	base                          string
	config                        Config
	file                          logHandle
	size                          int64
	rotated, temp                 *regexp.Regexp
	rename                        func(string, string) error
	remove                        func(string) error
	open                          func(string, bool) (*os.File, error)
	now                           func() time.Time
	retryAt, rotateAt, maintainAt time.Time
	backoff                       time.Duration
	counters                      *totals
}

func privateMode(info os.FileInfo, directory bool) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	allowed := os.FileMode(0600)
	if directory {
		allowed = 0700
	}
	return info.Mode().Perm()&^allowed == 0
}

func checkLogDirectorySpelling(dataDir string) error {
	entries, err := os.ReadDir(filepath.Clean(dataDir))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "logs" && strings.EqualFold(entry.Name(), "logs") {
			return invalid("path_invalid")
		}
	}
	return nil
}

func openWriter(dataDir string, c Config, counters *totals) (*fileWriter, error) {
	directory, base, err := c.path(dataDir)
	if err != nil {
		return nil, err
	}
	if err = checkLogDirectorySpelling(dataDir); err != nil {
		return nil, err
	}
	// Validate existing reserved-directory permissions before creating descendants.
	logRoot := filepath.Join(dataDir, "logs")
	rel, err := filepath.Rel(logRoot, directory)
	if err != nil {
		return nil, invalid("path_invalid")
	}
	path := logRoot
	for _, part := range append([]string{""}, strings.Split(rel, string(filepath.Separator))...) {
		if part != "" && part != "." {
			path = filepath.Join(path, part)
		}
		info, e := os.Lstat(path)
		if e == nil {
			if !info.IsDir() || !privateMode(info, true) {
				return nil, invalid("permission")
			}
		} else if !os.IsNotExist(e) {
			return nil, e
		}
	}
	canonical, identity, pin, err := pathguard.EnsureDirectory(directory, 0700, func(canonical, operation string, info os.FileInfo) (pathguard.Pin, error) {
		return pathguard.NewPin(canonical, operation, info, pathguard.PinLabels{Store: "access log", Access: "access log", Invalid: os.ErrInvalid})
	}, nil)
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*fileWriter, error) { pin.Close(); return nil, e }
	// Recheck after creation: case-insensitive lookup must not adopt a raced alias.
	if err = checkLogDirectorySpelling(dataDir); err != nil {
		return fail(err)
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return fail(err)
	}
	dir, err := root.Open(".")
	if err != nil {
		root.Close()
		return fail(err)
	}
	opened, err := dir.Stat()
	if err != nil || !os.SameFile(identity, opened) {
		dir.Close()
		root.Close()
		return fail(invalid("path_invalid"))
	}
	w := &fileWriter{root: root, dir: dir, pin: pin, base: base, config: c, counters: counters, now: time.Now, backoff: time.Second}
	w.rotated = regexp.MustCompile("^" + regexp.QuoteMeta(base) + `\.([0-9]{8}T[0-9]{6}\.[0-9]{9}Z)-([0-9]{6})\.rot$`)
	w.temp = regexp.MustCompile(`^\.` + regexp.QuoteMeta(base) + `\.asb-access-preflight-[0-9a-f]{32}\.tmp$`)
	w.rename = func(from, to string) error {
		if err := pin.Validate(); err != nil {
			return err
		}
		return renameExclusive(dir, from, to)
	}
	w.remove = root.Remove
	w.open = func(name string, create bool) (*os.File, error) {
		if err := pin.Validate(); err != nil {
			return nil, err
		}
		return openLogFile(root, dir, name, create)
	}
	if err = w.preflight(); err == nil {
		err = w.openActive(false)
	}
	if err != nil {
		w.close()
		return nil, err
	}
	w.prune(w.now())
	w.maintainAt = w.now().Add(time.Minute)
	return w, nil
}
func (w *fileWriter) preflight() error {
	stream, err := w.root.Open(".")
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		entries, e := stream.ReadDir(128)
		for _, entry := range entries {
			if !w.temp.MatchString(entry.Name()) {
				continue
			}
			info, e := w.root.Lstat(entry.Name())
			if e != nil {
				return e
			}
			if regular(info) {
				if e = w.remove(entry.Name()); e != nil {
					return e
				}
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
	}
	fromID, err := randomHex()
	if err != nil {
		return err
	}
	toID, err := randomHex()
	if err != nil {
		return err
	}
	from := "." + w.base + ".asb-access-preflight-" + fromID + ".tmp"
	to := "." + w.base + ".asb-access-preflight-" + toID + ".tmp"
	f, err := w.open(from, true)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		_ = w.remove(from)
		return err
	}
	if err = w.rename(from, to); err != nil {
		if cleanup := w.remove(from); cleanup != nil {
			return cleanup
		}
		return err
	}
	return w.remove(to)
}
func randomHex() (string, error) {
	const digits = "0123456789abcdef"
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	var out [32]byte
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&15]
	}
	return string(out[:]), nil
}
func (w *fileWriter) openActive(reopen bool) error {
	info, err := w.root.Lstat(w.base)
	create := os.IsNotExist(err)
	if err != nil && !create {
		return err
	}
	if !create && (!regular(info) || !privateMode(info, false)) {
		return invalid("permission")
	}
	f, err := w.open(w.base, create)
	if err != nil {
		return err
	}
	opened, e := f.Stat()
	current, e2 := w.root.Lstat(w.base)
	if e != nil || e2 != nil || !regular(opened) || !regular(current) || !os.SameFile(opened, current) || !privateMode(opened, false) {
		f.Close()
		return invalid("path_invalid")
	}
	w.file = f
	w.size = opened.Size()
	if w.size > 0 && w.size+1 <= 2*w.config.MaxSizeMiB*MiB {
		n, e := w.file.Write([]byte("\n"))
		w.size += int64(n)
		if e != nil || n != 1 {
			f.Close()
			w.file = nil
			return io.ErrShortWrite
		}
	}
	if reopen {
		w.counters.reopens.Add(1)
	}
	w.backoff = time.Second
	w.retryAt = time.Time{}
	return nil
}
func (w *fileWriter) close() error {
	var err error
	if w.file != nil {
		err = errors.Join(w.file.Sync(), w.file.Close())
		w.file = nil
	}
	if w.dir != nil {
		err = errors.Join(err, w.dir.Close())
	}
	if w.root != nil {
		err = errors.Join(err, w.root.Close())
	}
	if w.pin != nil {
		err = errors.Join(err, w.pin.Close())
	}
	return err
}
func (w *fileWriter) fail(now time.Time) {
	w.counters.writeErrors.Add(1)
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	w.retryAt = now.Add(w.backoff)
	w.backoff *= 2
	if w.backoff > 30*time.Second {
		w.backoff = 30 * time.Second
	}
}
func (w *fileWriter) rotate(now time.Time) error {
	if w.file == nil {
		return os.ErrClosed
	}
	if err := w.file.Close(); err != nil {
		w.file = nil
		w.fail(now)
		return err
	}
	w.file = nil
	stamp := now.UTC().Format("20060102T150405.000000000Z")
	for sequence := 1; sequence <= 1001; sequence++ {
		name := w.base + "." + stamp + "-" + sequenceName(sequence) + ".rot"
		err := w.rename(w.base, name)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		w.rotateAt = time.Time{}
		if err = w.openActive(false); err != nil {
			w.fail(now)
			return err
		}
		w.prune(now)
		return nil
	}
	return os.ErrExist
}
func sequenceName(n int) string {
	var out [6]byte
	for i := 5; i >= 0; i-- {
		out[i] = byte(n%10) + '0'
		n /= 10
	}
	return string(out[:])
}
func (w *fileWriter) prepare(size int, now time.Time) bool {
	if w.file == nil {
		if now.Before(w.retryAt) {
			return false
		}
		if err := w.openActive(true); err != nil {
			w.fail(now)
			return false
		}
	}
	limit := w.config.MaxSizeMiB * MiB
	if w.size+int64(size) > limit {
		if w.rotateAt.IsZero() || !now.Before(w.rotateAt) {
			if err := w.rotate(now); err != nil {
				w.counters.rotationErrors.Add(1)
				w.rotateAt = now.Add(30 * time.Second)
				if w.file == nil && w.retryAt.IsZero() {
					if err = w.openActive(true); err != nil {
						w.fail(now)
					}
				}
			}
		}
	}
	if w.file == nil || w.size+int64(size) > 2*limit {
		return false
	}
	return true
}

func (w *fileWriter) writeData(line []byte, now time.Time) bool {
	if err := w.pin.Validate(); err != nil {
		w.fail(now)
		return false
	}
	if w.file == nil || w.size+int64(len(line)) > 2*w.config.MaxSizeMiB*MiB {
		return false
	}
	n, err := w.file.Write(line)
	w.size += int64(n)
	if err != nil || n != len(line) {
		w.fail(now)
		return false
	}
	return true
}
func (w *fileWriter) write(line []byte, now time.Time) bool {
	return w.prepare(len(line), now) && w.writeData(line, now)
}

func (w *fileWriter) maintenance(now time.Time) {
	if err := w.pin.Validate(); err != nil {
		if w.file != nil || !now.Before(w.retryAt) {
			w.fail(now)
		}
		return
	}
	if w.file == nil && !now.Before(w.retryAt) {
		if err := w.openActive(true); err != nil {
			w.fail(now)
		}
	}
	if !w.rotateAt.IsZero() && !now.Before(w.rotateAt) && w.file != nil {
		if err := w.rotate(now); err != nil {
			w.counters.rotationErrors.Add(1)
			w.rotateAt = now.Add(30 * time.Second)
			if w.file == nil && w.retryAt.IsZero() {
				if err = w.openActive(true); err != nil {
					w.fail(now)
				}
			}
		}
	}
	if now.Before(w.maintainAt) {
		return
	}
	w.maintainAt = now.Add(time.Minute)
	if w.file != nil {
		handle, e := w.file.Stat()
		named, e2 := w.root.Lstat(w.base)
		if e != nil || (e2 != nil && !os.IsNotExist(e2)) {
			w.fail(now)
		} else if os.IsNotExist(e2) || !regular(named) || !os.SameFile(handle, named) {
			if err := w.file.Close(); err != nil {
				w.file = nil
				w.fail(now)
			} else {
				w.file = nil
				if err = w.openActive(true); err != nil {
					w.fail(now)
				}
			}
		}
	}
	w.prune(now)
}

type rotation struct {
	name string
	at   time.Time
}
type rotations []rotation

func (h rotations) Len() int           { return len(h) }
func (h rotations) Less(i, j int) bool { return h[i].name < h[j].name }
func (h rotations) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *rotations) Push(v any)        { *h = append(*h, v.(rotation)) }
func (h *rotations) Pop() any          { old := *h; v := old[len(old)-1]; *h = old[:len(old)-1]; return v }
func (w *fileWriter) prune(now time.Time) {
	if err := w.pin.Validate(); err != nil {
		w.counters.pruneErrors.Add(1)
		return
	}
	stream, err := w.root.Open(".")
	if err != nil {
		w.counters.pruneErrors.Add(1)
		return
	}
	defer stream.Close()
	candidates := &rotations{}
	cutoff := now.UTC().Add(-time.Duration(w.config.MaxAgeDays) * 24 * time.Hour)
	remove := func(name string) bool {
		if err := w.remove(name); err != nil {
			w.counters.pruneErrors.Add(1)
			return false
		}
		return true
	}
	for {
		entries, e := stream.ReadDir(128)
		for _, entry := range entries {
			matches := w.rotated.FindStringSubmatch(entry.Name())
			if matches == nil {
				continue
			}
			at, err := time.Parse("20060102T150405.000000000Z", matches[1])
			if err != nil {
				continue
			}
			info, err := w.root.Lstat(entry.Name())
			if err != nil {
				w.counters.pruneErrors.Add(1)
				return
			}
			if !regular(info) {
				continue
			}
			if at.Before(cutoff) {
				if !remove(entry.Name()) {
					return
				}
				continue
			}
			heap.Push(candidates, rotation{entry.Name(), at})
			if candidates.Len() > w.config.MaxFiles {
				old := heap.Pop(candidates).(rotation)
				if !remove(old.name) {
					return
				}
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			w.counters.pruneErrors.Add(1)
			return
		}
	}
}
