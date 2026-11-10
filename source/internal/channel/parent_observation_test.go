package channel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestStartupParentSingleObservation(t *testing.T) {
	for _, firstOK := range []bool{false, true} {
		t.Run(map[bool]string{false: "fail-then-succeed", true: "succeed-then-fail"}[firstOK], func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
			p := duplicateParent(t)
			p.ancestors = func() ([]int, error) { return []int{61530}, nil }
			var calls atomic.Int32
			p.classify = func(int) (string, error) {
				if (calls.Add(1) == 1) == firstOK {
					return "claude", nil
				}
				return "", errors.New("synthetic unavailable")
			}
			cs := launchHintClient(t, []string{"--broker-url", "http://127.0.0.1:1/mcp?project=test", "--token", "dummy", "--state-dir", t.TempDir()}, p)
			if got := calls.Load(); got != 1 {
				t.Fatalf("classification passes=%d, want 1", got)
			}
			if !firstOK {
				for i := 0; i < 2; i++ {
					r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "auth_status", Arguments: struct{}{}})
					if err != nil {
						t.Fatal(err)
					}
					b, _ := json.Marshal(r.StructuredContent)
					var status authStatus
					_ = json.Unmarshal(b, &status)
					if status.Status != "unsupported-harness" || status.Registered {
						t.Fatal("failed observation was replaced")
					}
				}
				if calls.Load() != 1 {
					t.Fatal("auth_status re-observed parent")
				}
			}
		})
	}
}

func TestTerminalParentFailureBlocksIdentityAndRegistration(t *testing.T) {
	for _, device := range []bool{false, true} {
		t.Run(map[bool]string{false: "token", true: "device"}[device], func(t *testing.T) {
			var identify, sink, registration atomic.Int32
			opts := (durableOptions{enabled: true, tokenMode: true, identityFailure: errUnsupportedHarness}).withDefaults()
			if device {
				opts.device = &deviceAuthOptions{}
			}
			s := &ingressServer{ctx: t.Context(), upstream: &ingressUpstream{}, opts: opts, bindings: map[string]*ingressBinding{},
				identify: func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) {
					identify.Add(1)
					return broker.AttachRequest{Harness: "claude", SessionID: "later-valid", PID: 61530, CWD: "/fixture"}, nil
				},
				factory: func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
					sink.Add(1)
					return nil, nil, nil
				},
			}
			s.attach = func(context.Context, *mcp.CallToolRequest, string, registerFunc) (broker.Session, error) {
				registration.Add(1)
				return broker.Session{}, nil
			}
			if _, err := s.observeIdentity(t.Context(), nil); !errors.Is(err, errUnsupportedHarness) {
				t.Fatal("identify did not fail closed")
			}
			if _, err := s.prepare(t.Context(), nil); !errors.Is(err, errUnsupportedHarness) {
				t.Error("prepare did not fail closed")
			}
			if err := s.autoAttach(t.Context(), nil); !errors.Is(err, errUnsupportedHarness) {
				t.Error("automatic admission did not fail closed")
			}
			s.automatic()
			s.workers.Wait()
			if identify.Load() != 0 || sink.Load() != 0 || registration.Load() != 0 || len(s.bindings) != 0 {
				t.Fatal("terminal failure reached later identity, sink, admission or resume")
			}
		})
	}
}

func TestWindowsNativeImageName(t *testing.T) {
	for _, tc := range []struct{ image, want string }{
		{`\Device\HarddiskVolume3\tools\claude.exe`, "claude"},
		{`\Device\HarddiskVolume3\tools\CoDeX.ExE`, "codex"},
		{`C:\tools\claude.exe.old.123.42`, ""},
		{`\Device\Volume\claude.exe.old.123.42`, ""},
		{`\Device\Volume\node.exe`, ""},
	} {
		if got := harnessFromWindowsImage(tc.image); got != tc.want {
			t.Errorf("native classifier got=%q want=%q", got, tc.want)
		}
	}
}

func TestParentDiagnosticOriginalObservation(t *testing.T) {
	var calls int
	p := clientParentSource{ancestors: func() ([]int, error) { return []int{11, 22, 33}, nil }, classifyObserved: func(pid int) (string, string, error) {
		calls++
		if pid == 11 {
			return "", `C:\private\node.exe`, nil
		}
		if calls == 2 {
			return "", strings.Repeat("hostile\u202e", 1000), parentFailure("query", syscall.Errno(5))
		}
		return "claude", "claude.exe", nil
	}}
	_, h, pid, d := observeClientParent(p)
	if h != "" || pid != 0 || d == nil || d.Step != "query" || d.Errno != 5 || calls != 2 {
		t.Fatal("original failure or nearest-first stop lost")
	}
	if len(d.Entries) != 2 || d.Entries[0].Label != "node.exe" || d.Entries[1].Label != "other" {
		t.Fatal("diagnostic exposed raw names")
	}
	raw, _ := json.Marshal(d)
	if len(raw) > 2048 {
		t.Fatal("diagnostic too large")
	}
}

func TestParentDiagnosticLabelsAndBounds(t *testing.T) {
	for _, tc := range []struct{ input, label string }{
		{`C:\private\CLAUDE.EXE`, "claude.exe"}, {"codex-other-binary", "codex-other"}, {"claude.exe.old.123.4", "claude-other"},
		{strings.Repeat("private", 10000), "other"}, {"asb_" + "p_" + strings.Repeat("a", 32) + ".exe", "other"}, {"evil\nnode.exe", "other"},
	} {
		if parentImageLabel(tc.input) != tc.label {
			t.Fatal("label is outside safe projection")
		}
	}
	for _, cycle := range []bool{false, true} {
		parents := make([]int, 16)
		for i := range parents {
			parents[i] = i + 1
		}
		if cycle {
			parents[2] = 1
		}
		calls := 0
		p := clientParentSource{ancestors: func() ([]int, error) { return parents, nil }, classify: func(int) (string, error) { calls++; return "", nil }}
		_, _, _, d := observeClientParent(p)
		raw, _ := json.Marshal(d)
		if calls > 16 || len(d.Entries) > 16 || len(raw) > 2048 {
			t.Fatal("unbounded diagnostic")
		}
		if cycle && d.Step != "cycle" || !cycle && d.Step != "limit" {
			t.Fatal("termination reason lost")
		}
	}
}

func TestParentDiagnosticNearestInnerAndSuccessOmission(t *testing.T) {
	calls := 0
	p := clientParentSource{ancestors: func() ([]int, error) { return []int{11, 22}, nil }, classifyObserved: func(int) (string, string, error) {
		calls++
		return harnessFromWindowsImage(`\Device\Volume\claude.exe`), "claude.exe", nil
	}}
	_, h, pid, d := observeClientParent(p)
	if h != "claude" || pid != 11 || calls != 1 || d != nil {
		t.Fatal("inner parent was skipped or success gained diagnostic")
	}
	raw, _ := json.Marshal(authStatus{Status: "ready"})
	if strings.Contains(string(raw), "parentDiagnostic") {
		t.Fatal("success bytes gained optional diagnostic")
	}
}

func TestTerminalParentFailureLiveIngressModes(t *testing.T) {
	for _, device := range []bool{false, true} {
		t.Run(map[bool]string{false: "configured-token", true: "device"}[device], func(t *testing.T) {
			auth := newDeviceAuthServer(t)
			up := newSwitchableUpstream(t)
			var sinks atomic.Int32
			f := newDeviceModeFixture(t, auth, up, t.TempDir(), singleIdentity(), func(f *deviceModeFixture) {
				f.deferAttach = true
				f.sharedUpstream = &recordingUpstream{inner: up, log: f.log}
				f.sinkFactory = func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
					sinks.Add(1)
					return nil, nil, errors.New("unexpected sink")
				}
				f.configureDurable = func(o *durableOptions) {
					o.identityFailure = errUnsupportedHarness
					if !device {
						o.device = nil
					}
				}
			})
			for i := 0; i < 2; i++ {
				r := f.call("", "auth_status", map[string]any{})
				raw, _ := json.Marshal(r.StructuredContent)
				var status authStatus
				_ = json.Unmarshal(raw, &status)
				if status.Status != "unsupported-harness" || status.Registered {
					t.Fatal("identity failure status changed")
				}
			}
			r := f.call("", "attach_session", map[string]any{})
			if !r.IsError {
				t.Fatal("explicit attach passed terminal failure")
			}
			if f.codeRequests() != 0 || f.tokenRequests() != 0 || f.log.count("register_session") != 0 || f.log.count("resume_session") != 0 || sinks.Load() != 0 {
				t.Fatal("failed startup caused admission, resume or sink effects")
			}
		})
	}
}
