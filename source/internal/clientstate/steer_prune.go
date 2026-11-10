package clientstate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Store) pruneSteerAttempts(now time.Time, result *PruneResult) {
	parent := filepath.Join(s.dir, "steer-attempts", "v1")
	for i, dir := range []string{s.dir, filepath.Dir(parent), parent} {
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil || !info.IsDir() || !privateSteerPath(dir, info, i > 0) {
			result.Skipped++
			return
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		result.Errors++
		return
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".lock")
		if !strings.HasSuffix(name, ".json") || !steerDigest.MatchString(strings.TrimSuffix(name, ".json")) {
			result.Skipped++
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		removed, err := pruneSteerAttempt(filepath.Join(parent, name), now)
		switch {
		case err != nil:
			result.Errors++
		case removed:
			result.SteerAttempts++
		default:
			result.Skipped++
		}
	}
}

// The open file, not a second path lookup alone, must own this exact private inode.
func samePrivateSteerFile(path string, file *os.File) (os.FileInfo, bool) {
	current, ok := plainPath(path, false)
	if !ok || !privateSteerPath(path, current, true) {
		return nil, false
	}
	held, err := file.Stat()
	return current, err == nil && os.SameFile(current, held) && privateSteerPath(path, held, true)
}

func pruneSteerAttempt(path string, now time.Time) (bool, error) {
	lock := path + ".lock"
	before, ok := plainPath(lock, false)
	if !ok || !safeLockPath(lock) || !privateSteerPath(lock, before, true) {
		return false, nil
	}
	file, release, err := lockExistingSteerFile(lock)
	if err != nil {
		return false, nil
	}
	defer release()
	held, ok := samePrivateSteerFile(lock, file)
	if !ok || !os.SameFile(before, held) {
		return false, nil
	}
	data, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if now.Sub(held.ModTime()) <= sessionIdle {
			return false, nil
		}
	case err != nil:
		return false, nil
	default:
		if !data.Mode().IsRegular() || !privateSteerPath(path, data, true) || now.Sub(data.ModTime()) <= sessionIdle {
			return false, nil
		}
		input, err := os.Open(path)
		if err != nil {
			return false, nil
		}
		defer input.Close()
		current, ok := samePrivateSteerFile(path, input)
		if !ok || !os.SameFile(data, current) {
			return false, nil
		}
		record, err := decodeSteerRecord(input)
		// shortcut: pending records never expire, add retirement-aware GC if dead threads accumulate.
		// Raw scope is unavailable: validate structure, not filename-to-scope binding.
		if err != nil || record.Pending != nil {
			return false, nil
		}
		current, ok = samePrivateSteerFile(path, input)
		if !ok || !os.SameFile(data, current) || now.Sub(current.ModTime()) <= sessionIdle {
			return false, nil
		}
	}
	if current, ok := samePrivateSteerFile(lock, file); !ok || !os.SameFile(held, current) {
		return false, nil
	}
	if data != nil {
		if err := os.Remove(path); err != nil {
			return false, err
		}
	} else if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	// No further data access after unlinking this lock; old waiters fail closed.
	if err := os.Remove(lock); err != nil {
		return false, err
	}
	return true, nil
}
