package channel

import (
	"path/filepath"
	"regexp"
	"strings"
)

var claudeExecutableVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*$`)

// harnessFromExecutable classifies a Unix executable path without reading a
// process's command line or environment.
func harnessFromExecutable(executable string) string {
	name := filepath.Base(executable)
	if name == "claude" || name == "codex" {
		return name
	}
	parent := filepath.Dir(executable)
	if filepath.Base(parent) == "versions" && filepath.Base(filepath.Dir(parent)) == "claude" && claudeExecutableVersion.MatchString(name) {
		return "claude"
	}
	return ""
}

// A Node runtime alone is never a supported harness.
func harnessFromProcess(executable, entry, title string) string {
	if h := harnessFromExecutable(executable); h != "" {
		return h
	}
	if filepath.Base(executable) != "node" {
		return ""
	}
	// shortcut: process title can be set by any local node script; acceptable because the harness label is not an authorization boundary (token + hook-bound conversation still required); upgrade if Kimi exposes a verifiable marker
	if title == "kimi-code" {
		return "kimi"
	}
	if entry == "" || strings.HasPrefix(entry, "-") {
		return ""
	}
	real, err := filepath.EvalSymlinks(entry)
	if err == nil && strings.HasSuffix(filepath.ToSlash(real), "/@moonshot-ai/kimi-code/dist/main.mjs") {
		return "kimi"
	}
	return ""
}

// harnessFromWindowsImage accepts the image returned by the live process query.
func harnessFromWindowsImage(image string) string {
	switch strings.ToLower(image[strings.LastIndexByte(image, '\\')+1:]) {
	case "claude.exe":
		return "claude"
	case "codex.exe":
		return "codex"
	}
	return ""
}
