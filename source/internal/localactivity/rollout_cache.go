package localactivity

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xelexis-dev/agent-session-broker/source/internal/pathguard"
)

func (o *Observer) cachedRollout(ctx context.Context, now time.Time) (string, error) {
	if ctx.Err() != nil {
		return "", unavailable
	}
	if !now.Before(o.nextLocate) {
		path, err := o.locateRollout(ctx)
		o.located, o.locateErr = path, err
		delay := time.Minute
		if err == missing {
			o.locateBackoff = min(max(5*time.Second, 2*o.locateBackoff), time.Minute)
			delay = o.locateBackoff
		} else {
			o.locateBackoff = 0
		}
		o.nextLocate = now.Add(delay)
	}
	if o.locateErr != nil {
		return "", o.locateErr
	}
	if err := o.recentRolloutDuplicate(ctx, now); err != nil {
		// A recent scan cannot restore uniqueness after any known scan failure.
		o.located, o.locateErr = "", err
		return "", err
	}
	return o.located, nil
}

func (o *Observer) recentRolloutDuplicate(ctx context.Context, now time.Time) error {
	base := filepath.Join(o.config.Home, "sessions")
	if _, _, err := pathguard.ValidateDirectoryPath(base); err != nil {
		return missing
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return missing
	}
	defer root.Close()
	relative, err := filepath.Rel("sessions", o.located)
	if err != nil {
		return mismatch
	}
	cached, err := root.Lstat(relative)
	if err != nil {
		return missing
	}
	today := now.In(time.Local)
	for _, date := range []time.Time{today, today.AddDate(0, 0, -1)} {
		if ctx.Err() != nil {
			return unavailable
		}
		dir := date.Format("2006/01/02")
		if _, _, err := pathguard.ValidateDirectoryPath(filepath.Join(base, dir)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return malformed
		}
		f, err := root.Open(dir)
		if err != nil {
			return missing
		}
		entries, err := f.ReadDir(maxEntries + 1)
		f.Close()
		if err != nil && err != io.EOF {
			return missing
		}
		if len(entries) > maxEntries {
			return budgetExceeded
		}
		for _, entry := range entries {
			if ctx.Err() != nil {
				return unavailable
			}
			name := entry.Name()
			if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, "-"+o.config.Binding.SessionID+".jsonl") {
				continue
			}
			path := filepath.Join(dir, name)
			if filepath.Clean(path) == filepath.Clean(relative) {
				continue
			}
			info, err := root.Lstat(path)
			if err != nil {
				return missing
			}
			if !info.Mode().IsRegular() || aliasFile(info) {
				return malformed
			}
			if !os.SameFile(info, cached) {
				return mismatch
			}
		}
	}
	return nil
}
