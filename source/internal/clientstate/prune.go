package clientstate

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	sessionIdle    = 30 * 24 * time.Hour
	diagnosticsAge = 14 * 24 * time.Hour
	pruneInterval  = 24 * time.Hour
)

// PruneResult contains counts only; filenames and credential contents are never reported.
type PruneResult struct {
	Sessions      int
	Registrations int
	Diagnostics   int
	SteerAttempts int
	Skipped       int
	Errors        int
}

// plainPath rejects symlinks and unexpected file types before any lock/open.
func plainPath(path string, directory bool) (fs.FileInfo, bool) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return nil, false
	}
	return info, info.IsDir() == directory && (directory || info.Mode().IsRegular())
}

func safeLockPath(path string) bool {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	_, ok := plainPath(path, false)
	return ok
}

// Prune removes only recognized, idle direct children of the local state store.
// Only steer attempts require bounded strict decoding; no live lock is awaited.
func (s *Store) Prune(now time.Time) (result PruneResult, err error) {
	fail := func() { result.Errors++ }
	defer func() {
		if result.Errors > 0 {
			err = errors.New("client state pruning incomplete")
		}
	}()
	if _, ok := plainPath(s.dir, true); !ok {
		fail()
		return
	}
	lock := filepath.Join(s.dir, "prune.lock")
	if !safeLockPath(lock) {
		result.Skipped++
		return
	}
	unlock, e := lockFile(lock)
	if errors.Is(e, ErrLocked) {
		result.Skipped++
		return
	}
	if e != nil {
		fail()
		return
	}
	defer unlock()
	stamp := filepath.Join(s.dir, "prune.stamp")
	if info, e := os.Lstat(stamp); e == nil {
		if !info.Mode().IsRegular() {
			result.Skipped++
			return
		}
		if now.Sub(info.ModTime()) < pruneInterval {
			result.Skipped++
			return
		}
	} else if !errors.Is(e, fs.ErrNotExist) {
		fail()
		return
	}
	for _, kind := range []string{"sessions", "registrations", "diagnostics", "diagnostics/v2", "steer-attempts/v1"} {
		if kind == "steer-attempts/v1" {
			s.pruneSteerAttempts(now, &result)
			continue
		}
		if kind == "diagnostics/v2" {
			if _, ok := plainPath(filepath.Join(s.dir, "diagnostics"), true); !ok {
				result.Skipped++
				continue
			}
		}
		parent := filepath.Join(s.dir, kind)
		info, e := os.Lstat(parent)
		if errors.Is(e, fs.ErrNotExist) {
			continue
		}
		if e != nil {
			fail()
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			result.Skipped++
			continue
		}
		entries, e := os.ReadDir(parent)
		if e != nil {
			fail()
			continue
		}
		for _, entry := range entries {
			key := entry.Name()
			if kind != "sessions" {
				if !strings.HasSuffix(key, ".json") {
					continue
				}
				key = strings.TrimSuffix(key, ".json")
			}
			if !diagnosticKey.MatchString(key) {
				result.Skipped++
				continue
			}
			target := filepath.Join(parent, entry.Name())
			before, ok := plainPath(target, kind == "sessions")
			if !ok {
				result.Skipped++
				continue
			}
			data, lockPath := target, filepath.Join(parent, key+".lock")
			diagnostic := kind == "diagnostics" || kind == "diagnostics/v2"
			age := sessionIdle
			if kind == "sessions" {
				data = filepath.Join(target, "conn.json")
				lockPath = filepath.Join(target, "conn.lock")
			}
			if diagnostic {
				age = diagnosticsAge
			}
			if !safeLockPath(lockPath) {
				result.Skipped++
				continue
			}
			var release func()
			if kind == "sessions" {
				release, e = s.LockSession(key)
			} else if kind == "registrations" {
				release, e = s.Lock(key)
			} else {
				release, e = lockFile(lockPath)
			}
			if errors.Is(e, ErrLocked) {
				result.Skipped++
				continue
			}
			if e != nil {
				fail()
				continue
			}
			current, ok := plainPath(target, kind == "sessions")
			if !ok || !os.SameFile(before, current) {
				release()
				result.Skipped++
				continue
			}
			modified := current.ModTime()
			if kind == "sessions" {
				c, e := os.Lstat(data)
				switch {
				case errors.Is(e, fs.ErrNotExist):
					// Creating conn.lock can update the directory mtime. Use its pre-lock
					// timestamp only when it is still the same directory and has no state.
					modified = before.ModTime()
				case e != nil:
					release()
					fail()
					continue
				case !c.Mode().IsRegular():
					release()
					result.Skipped++
					continue
				default:
					modified = c.ModTime()
				}
			}
			if now.Sub(modified) <= age {
				release()
				continue
			}
			// Windows cannot remove an open lock file. A failed removal after closing
			// is harmless and reported as a count; the next scheduled run retries it.
			if runtime.GOOS == "windows" {
				release()
			}
			if kind == "sessions" {
				e = os.RemoveAll(target)
			} else {
				e = os.Remove(target)
			}
			if e == nil && kind != "sessions" {
				e = os.Remove(lockPath)
			}
			if runtime.GOOS != "windows" {
				release()
			}
			if e != nil {
				fail()
				continue
			}
			switch kind {
			case "sessions":
				result.Sessions++
			case "registrations":
				result.Registrations++
			case "diagnostics", "diagnostics/v2":
				result.Diagnostics++
			}
		}
	}
	if e := writeFile(stamp, nil); e != nil {
		fail()
	} else if e := os.Chtimes(stamp, now, now); e != nil {
		fail()
	}
	return
}
