package localactivity

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/xelexis-dev/agent-session-broker/source/internal/pathguard"
)

const maxRead = 1 << 20
const maxEntries = 4096
const maxDateDirs = 4096

type readBudget struct {
	ctx  context.Context
	left int64
}

func (b *readBudget) readAt(f io.ReaderAt, offset, length int64) ([]byte, error) {
	if length < 0 || length > b.left {
		return nil, budgetExceeded
	}
	data := make([]byte, length)
	for done := int64(0); done < length; {
		if b.ctx.Err() != nil {
			return nil, unavailable
		}
		end := min(done+maxRead, length)
		n, err := f.ReadAt(data[done:end], offset+done)
		b.left -= int64(n)
		done += int64(n)
		if err != nil {
			return nil, missing
		}
	}
	return data, nil
}

type evidenceFile struct {
	root       *os.Root
	file       *os.File
	info       os.FileInfo
	home, path string
}

func (f *evidenceFile) close() { f.file.Close(); f.root.Close() }
func (f *evidenceFile) validate() error {
	if _, _, err := pathguard.ValidateDirectoryPath(filepath.Dir(filepath.Join(f.home, f.path))); err != nil {
		return malformed
	}
	link, err := f.root.Lstat(f.path)
	if err != nil || !link.Mode().IsRegular() || aliasFile(link) || !os.SameFile(link, f.info) {
		return malformed
	}
	after, err := f.file.Stat()
	if err != nil || after.Size() != f.info.Size() || !after.ModTime().Equal(f.info.ModTime()) {
		return malformed
	}
	return nil
}

func (o *Observer) openEvidence(relative string) (*evidenceFile, error) {
	home := o.config.Home
	if home == "" || !filepath.IsLocal(relative) {
		return nil, mismatch
	}
	if _, _, err := pathguard.ValidateDirectoryPath(filepath.Dir(filepath.Join(home, relative))); err != nil {
		return nil, missing
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, missing
	}
	link, err := root.Lstat(relative)
	if err != nil || !link.Mode().IsRegular() || aliasFile(link) {
		root.Close()
		return nil, malformed
	}
	file, err := root.OpenFile(relative, readFlags(), 0)
	if err != nil {
		root.Close()
		return nil, missing
	}
	info, err := file.Stat()
	f := &evidenceFile{root: root, file: file, info: info, home: home, path: relative}
	if err != nil || !info.Mode().IsRegular() || aliasFile(info) || !os.SameFile(info, link) {
		f.close()
		return nil, malformed
	}
	if err = f.validate(); err != nil {
		f.close()
		return nil, err
	}
	if o.fileInfo != nil && (!os.SameFile(o.fileInfo, info) || o.selected != relative) {
		o.blocked = true
		f.close()
		return nil, malformed
	}
	if o.fileInfo != nil && (o.config.Binding.Harness == "codex" || o.config.RuntimeInfo && o.config.Format == "") && info.Size() < o.fileInfo.Size() {
		o.blocked = true
		f.close()
		return nil, malformed
	}
	if o.fileInfo != nil && (o.config.Binding.Harness == "codex" || o.config.RuntimeInfo && o.config.Format == "") && info.Size() == o.fileInfo.Size() && !info.ModTime().Equal(o.fileInfo.ModTime()) {
		o.blocked = true
		f.close()
		return nil, malformed
	}
	o.fileInfo, o.selected = info, relative
	return f, nil
}

// Selection stays inside sessions/YYYY/MM/DD, with per-directory entry and
// global date-directory budgets.
// Cached observation revalidates this full uniqueness check at least every minute.
func (o *Observer) locateRollout(ctx context.Context) (string, error) {
	base := filepath.Join(o.config.Home, "sessions")
	if _, _, err := pathguard.ValidateDirectoryPath(base); err != nil {
		return "", missing
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return "", missing
	}
	defer root.Close()
	dateDirs := 0
	found := ""
	var walk func(string, int) error
	walk = func(dir string, depth int) error {
		if ctx.Err() != nil {
			return unavailable
		}
		if depth == 3 {
			dateDirs++
			if dateDirs > maxDateDirs {
				return budgetExceeded
			}
		}
		if _, _, err := pathguard.ValidateDirectoryPath(filepath.Join(base, dir)); err != nil {
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
			name := entry.Name()
			if depth < 3 {
				width := 2
				if depth == 0 {
					width = 4
				}
				if len(name) != width || strings.Trim(name, "0123456789") != "" {
					continue
				}
				if entry.Type()&os.ModeSymlink != 0 {
					return malformed
				}
				if !entry.IsDir() {
					continue
				}
				if err := walk(filepath.Join(dir, name), depth+1); err != nil {
					return err
				}
			} else if strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, "-"+o.config.Binding.SessionID+".jsonl") {
				if !entry.Type().IsRegular() {
					return malformed
				}
				candidate := filepath.Join("sessions", dir, name)
				if found != "" {
					first, e1 := root.Lstat(strings.TrimPrefix(found, "sessions"+string(filepath.Separator)))
					second, e2 := root.Lstat(filepath.Join(dir, name))
					if e1 != nil || e2 != nil {
						return missing
					}
					if !os.SameFile(first, second) {
						return mismatch
					}
					if candidate == o.selected {
						found = candidate
					}
					continue
				}
				found = filepath.Join("sessions", dir, name)
			}
		}
		return nil
	}
	if err := walk(".", 0); err != nil {
		return "", err
	}
	if found == "" {
		return "", missing
	}
	return filepath.Clean(found), nil
}
