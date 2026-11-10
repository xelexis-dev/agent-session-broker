package channel

import (
	"errors"
	"runtime"
	"strings"
	"syscall"
)

type parentDiagnostic struct {
	OS      string                  `json:"os"`
	Step    string                  `json:"step"`
	Errno   uint32                  `json:"errno"`
	Entries []parentDiagnosticEntry `json:"entries,omitempty"`
}
type parentDiagnosticEntry struct {
	Index int    `json:"index"`
	Label string `json:"label"`
}

// Only fixed labels survive the observation; image names and OS errors never do.
func parentImageLabel(image string) string {
	name := strings.ToLower(image[strings.LastIndexAny(image, `/\`)+1:])
	switch name {
	case "claude.exe", "codex.exe", "cmd.exe", "node.exe", "powershell.exe", "pwsh.exe":
		return name
	}
	if strings.HasPrefix(name, "claude") {
		return "claude-other"
	}
	if strings.HasPrefix(name, "codex") {
		return "codex-other"
	}
	return "other"
}

type parentObservationError struct {
	step  string
	errno uint32
	cause error
}

func (e *parentObservationError) Unwrap() error { return e.cause }
func (e *parentObservationError) Error() string { return "parent observation unavailable" }
func parentFailure(step string, err error) error {
	var errno syscall.Errno
	_ = errors.As(err, &errno)
	return &parentObservationError{step: step, errno: uint32(errno), cause: err}
}
func (d *parentDiagnostic) failure(err error) {
	var failure *parentObservationError
	if errors.As(err, &failure) {
		d.Step, d.Errno = failure.step, failure.errno
	}
}
func processHarness(pid int) (string, error) { h, _, e := processHarnessObserved(pid); return h, e }

func observeClientParent(p clientParentSource) ([]int, string, int, *parentDiagnostic) {
	d := &parentDiagnostic{OS: runtime.GOOS, Step: "snapshot"}
	ancestors, err := p.ancestors()
	if err != nil {
		d.failure(err)
		return ancestors, "", 0, d
	}
	d.Step = "matchName"
	seen := map[int]bool{}
	for i, pid := range ancestors {
		if i == 16 {
			d.Step = "limit"
			break
		}
		if seen[pid] {
			d.Step = "cycle"
			break
		}
		seen[pid] = true
		var h, label string
		if p.classifyObserved != nil {
			h, label, err = p.classifyObserved(pid)
		} else {
			h, err = p.classify(pid)
			label = parentImageLabel(h + ".exe")
		}
		d.Entries = append(d.Entries, parentDiagnosticEntry{Index: i, Label: parentImageLabel(label)})
		if err != nil {
			d.failure(err)
			return ancestors, "", 0, d
		}
		if h != "" {
			return ancestors, h, pid, nil
		}
	}
	if len(ancestors) == 16 && d.Step == "matchName" {
		d.Step = "limit"
	}
	return ancestors, "", 0, d
}
