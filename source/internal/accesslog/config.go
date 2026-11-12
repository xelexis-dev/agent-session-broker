// Package accesslog records bounded, classified HTTP events to server-owned files.
package accesslog

import (
	"errors"
	"path/filepath"
	"strings"
)

const MiB int64 = 1 << 20
const maxProduct int64 = 10 << 30

type Config struct {
	File                       string
	MaxSizeMiB                 int64
	MaxFiles, MaxAgeDays       int
	ForwardedFor, RetentionSet bool
}

func Defaults() Config { return Config{MaxSizeMiB: 10, MaxFiles: 10, MaxAgeDays: 30} }

type InitError struct{ Reason string }

func (e *InitError) Error() string { return "access_log_init_failed" }
func invalid(reason string) error  { return &InitError{Reason: reason} }
func (c Config) Validate() error {
	if c.File == "" {
		if c.ForwardedFor || c.RetentionSet {
			return invalid("configuration")
		}
		return nil
	}
	if c.File == "-" || c.MaxSizeMiB < 1 || c.MaxSizeMiB > 1024 || c.MaxFiles < 1 || c.MaxFiles > 1000 || c.MaxAgeDays < 1 || c.MaxAgeDays > 3650 {
		return invalid("configuration")
	}
	if c.MaxSizeMiB*MiB > maxProduct/int64(c.MaxFiles+1) {
		return invalid("configuration")
	}
	return nil
}
func (c Config) path(dataDir string) (string, string, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", "", invalid("path_invalid")
	}
	file := c.File
	if !filepath.IsAbs(file) {
		file = filepath.Join(abs, file)
	}
	file = filepath.Clean(file)
	rel, err := filepath.Rel(abs, file)
	if err != nil || !strings.HasPrefix(rel, "logs"+string(filepath.Separator)) {
		return "", "", invalid("path_invalid")
	}
	base := filepath.Base(file)
	if base == "." || base == ".." || strings.ContainsAny(base, "\x00\r\n") {
		return "", "", invalid("path_invalid")
	}
	return filepath.Dir(file), base, nil
}
func initFailure(err error) error {
	var e *InitError
	if errors.As(err, &e) {
		return e
	}
	return invalid("io")
}
