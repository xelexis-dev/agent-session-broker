// Package attachmentclient keeps file bytes outside model-facing messages.
package attachmentclient

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/xelexis-dev/agent-session-broker/source/internal/pathguard"
	pfs "github.com/xelexis-dev/agent-session-broker/source/internal/pinnedpath"
)

const MaxFileBytes int64 = 100 << 20

var ErrUnsupported = errors.New("unsupported")

func failure(code string) error { return errors.New(code) }

type Source struct {
	*os.File
	Size int64
	Name string
	pins []pathguard.Pin
}

func (s *Source) Close() error {
	err := s.File.Close()
	for i := len(s.pins) - 1; i >= 0; i-- {
		if e := s.pins[i].Close(); err == nil {
			err = e
		}
	}
	return err
}
func openPin(canonical, operation string, identity os.FileInfo) (pathguard.Pin, error) {
	return pathguard.NewPin(canonical, operation, identity, pathguard.PinLabels{Store: "attachment", Access: "attachment", Invalid: ErrUnsupported})
}

// OpenSource anchors every traversal step to the preceding directory handle.
func OpenSource(cwd, relative, state string) (*Source, error) {
	return openSource(cwd, relative, state, nil)
}
func openSource(cwd, relative, state string, barrier func(), rootBarrier ...func()) (*Source, error) {
	if strings.HasPrefix(relative, "/") || filepath.IsAbs(relative) || strings.HasPrefix(relative, "\\") || len(relative) > 1 && relative[1] == ':' {
		return nil, failure("attachment-absolute-path")
	}
	parts := strings.Split(strings.ReplaceAll(relative, "\\", "/"), "/")
	if relative == "" {
		return nil, failure("attachment-not-file")
	}
	for _, part := range parts {
		if part == ".." || part == "" || part == "." || strings.ContainsRune(part, 0) {
			return nil, failure("attachment-outside-project")
		}
		if invalidSpelling(part) {
			return nil, failure("attachment-outside-project")
		}
	}
	if denied(parts) {
		return nil, failure("attachment-credential")
	}
	rootFile, err := os.Open(cwd)
	if err != nil {
		return nil, failure("attachment-not-file")
	}
	defer rootFile.Close()
	info, err := rootFile.Stat()
	if err != nil || !info.IsDir() {
		return nil, failure("attachment-not-file")
	}
	canonical, err := rootLocation(rootFile)
	if err != nil {
		return nil, ErrUnsupported
	}
	home, _ := os.UserHomeDir()
	home, _ = canonicalCWD(home)
	if filepath.Dir(canonical) == canonical || home != "" && samePath(canonical, home) {
		return nil, failure("attachment-outside-project")
	}
	if inTree(state, filepath.Join(canonical, filepath.Join(parts...))) {
		return nil, failure("attachment-credential")
	}
	if len(rootBarrier) > 0 && rootBarrier[0] != nil {
		rootBarrier[0]()
	}
	currentInfo, resolved, err := pathguard.ValidateDirectoryPath(canonical)
	if err != nil || !samePath(resolved, canonical) || !os.SameFile(info, currentInfo) {
		return nil, failure("attachment-link")
	}
	pin, err := openPin(canonical, canonical, info)
	if err != nil {
		return nil, failure("attachment-link")
	}
	pin = &sourceRootPin{Pin: pin, canonical: canonical, identity: info}
	pins := []pathguard.Pin{pin}
	ok := false
	defer func() {
		if !ok {
			for i := len(pins) - 1; i >= 0; i-- {
				_ = pins[i].Close()
			}
		}
	}()
	current := canonical
	stateInfo, _ := os.Stat(state)
	if stateInfo != nil && os.SameFile(info, stateInfo) {
		return nil, failure("attachment-credential")
	}
	for _, part := range parts[:len(parts)-1] {
		next, identity, child, exists, e := pathguard.EnsureChild(pin, current, part, false, 0700, openPin, nil)
		if e != nil || !exists {
			return nil, failure("attachment-link")
		}
		pins = append(pins, child)
		if stateInfo != nil && os.SameFile(stateInfo, identity) {
			return nil, failure("attachment-credential")
		}
		pin, current = child, next
		if barrier != nil {
			barrier()
		}
	}
	name := parts[len(parts)-1]
	before, err := pfs.Lstat(filepath.Join(pin.Path(), name))
	if err != nil {
		return nil, failure("attachment-not-file")
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, failure("attachment-link")
	}
	if !before.Mode().IsRegular() {
		return nil, failure("attachment-not-file")
	}
	file, err := openLeaf(filepath.Join(pin.Path(), name))
	if err != nil {
		return nil, failure("attachment-link")
	}
	valid := false
	defer func() {
		if !valid {
			_ = file.Close()
		}
	}()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || pin.Validate() != nil {
		return nil, failure("attachment-link")
	}
	if err := validateLeaf(file, after, canonical, state); err != nil {
		return nil, err
	}
	if after.Size() < 1 || after.Size() > MaxFileBytes {
		return nil, failure("attachment-too-large")
	}
	valid, ok = true, true
	return &Source{File: file, Size: after.Size(), Name: SanitizeName(name), pins: pins}, nil
}
func samePath(a, b string) bool { return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) }
func inTree(root, path string) bool {
	if root == "" {
		return false
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return true
	}
	if resolved, e := filepath.EvalSymlinks(abs); e == nil {
		abs = resolved
	}
	rel, err := filepath.Rel(strings.ToLower(abs), strings.ToLower(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func invalidSpelling(part string) bool {
	if strings.ContainsAny(part, ":\x00") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
		return true
	}
	base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CLOCK$" {
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && strings.ContainsRune("123456789¹²³", rune(base[3]))
}
func denied(parts []string) bool {
	exact := "|.credentials|secrets|.mcp.json|.codex|.claude|.kimi|.ssh|.gnupg|.aws|.azure|.kube|.docker|.git|.netrc|_netrc|.npmrc|.pypirc|.git-credentials|.env|credentials|credentials.json|credentials.yaml|credentials.yml|admin-password|admin-password.txt|known_hosts|authorized_keys|"
	for i, p := range parts {
		p = strings.ToLower(p)
		if strings.Contains(exact, "|"+p+"|") || i > 0 && strings.EqualFold(parts[i-1], ".config") && p == "gcloud" {
			return true
		}
		for _, prefix := range []string{".env.", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"} {
			if strings.HasPrefix(p, prefix) {
				return true
			}
		}
		for _, suffix := range []string{".pem", ".key", ".p12", ".pfx", ".ppk", ".jks", ".keystore", ".kdbx"} {
			if strings.HasSuffix(p, suffix) {
				return true
			}
		}
	}
	return false
}
func SanitizeName(name string) string {
	var b strings.Builder
	for _, c := range name {
		if c > 127 || !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-", c)) {
			c = '_'
		}
		b.WriteByte(byte(c))
	}
	out := strings.TrimLeft(b.String(), ".")
	if len(out) > 64 {
		out = out[:64]
	}
	if out == "" {
		return "file"
	}
	return out
}

// Root validation includes ancestors even for a single-component source.
type sourceRootPin struct {
	pathguard.Pin
	canonical string
	identity  os.FileInfo
}

func (p *sourceRootPin) Validate() error {
	info, resolved, e := pathguard.ValidateDirectoryPath(p.canonical)
	if e != nil || !samePath(resolved, p.canonical) || !os.SameFile(p.identity, info) {
		return failure("attachment-link")
	}
	return p.Pin.Validate()
}
