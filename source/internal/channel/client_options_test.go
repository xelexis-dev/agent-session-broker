package channel

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestClientOptionsAcceptsProjectTokensForPolicyGuardedHTTP(t *testing.T) {
	legacy, err := parseClientOptions([]string{"--broker-url", "http://broker:8787/mcp?project=brk", "--token", "legacy-session-token"})
	if err != nil || legacy.deviceMode || legacy.token != "legacy-session-token" {
		t.Fatalf("legacy token over http: %+v %v", legacy, err)
	}
	projectHTTP, err := parseClientOptions([]string{"--broker-url", "http://broker:8787/mcp?project=brk", "--token", "asb_p_project"})
	if err != nil || !projectHTTP.deviceMode || projectHTTP.project != "brk" {
		t.Fatalf("project token over policy-guarded http: %+v %v", projectHTTP, err)
	}
	device, err := parseClientOptions([]string{"--broker-url", "https://broker.example/mcp?project=brk", "--token", "asb_p_project"})
	if err != nil || !device.deviceMode || device.project != "brk" {
		t.Fatalf("project token over https: %+v %v", device, err)
	}
	session, err := parseClientOptions([]string{"--broker-url", "https://broker.example/mcp?project=brk", "--token", "asb_s_session"})
	if err != nil || session.deviceMode {
		t.Fatalf("session credential must use the existing bearer flow: %+v %v", session, err)
	}
	for _, args := range [][]string{
		{"--broker-url", "https://broker.example/mcp?project=brk"},
		{"--broker-url", "https://broker.example/mcp?project=brk", "--token", ""},
		{"--broker-url", "https://broker.example/mcp?project=brk", "--token", "asb_p_x", "extra"},
		{"--broker-url", "https://broker.example/mcp?project=brk", "--token-file", "x"},
	} {
		if _, err := parseClientOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestClientOptionsTreatAuthDirAndStateDirAsOneSetting(t *testing.T) {
	base := []string{"--broker-url", "https://broker.example/mcp?project=brk", "--token", "asb_p_project"}
	// Absolute paths differ per platform; derive them from the test directory.
	dir := t.TempDir()
	other := filepath.Join(dir, "two")
	authOnly, err := parseClientOptions(append(base, "--auth-dir", dir))
	if err != nil || authOnly.stateDir != dir {
		t.Fatalf("--auth-dir: %+v %v", authOnly, err)
	}
	stateOnly, err := parseClientOptions(append(base, "--state-dir", dir))
	if err != nil || stateOnly.stateDir != dir {
		t.Fatalf("--state-dir: %+v %v", stateOnly, err)
	}
	same, err := parseClientOptions(append(base, "--auth-dir", dir, "--state-dir", dir))
	if err != nil || same.stateDir != dir {
		t.Fatalf("matching flags: %+v %v", same, err)
	}
	if _, err := parseClientOptions(append(base, "--auth-dir", dir, "--state-dir", other)); err == nil || !strings.Contains(err.Error(), "--auth-dir") {
		t.Fatalf("conflicting directories accepted: %v", err)
	}
	if _, err := parseClientOptions(append(base, "--auth-dir", filepath.Join("relative", "path"))); err == nil {
		t.Fatal("relative auth directory accepted")
	}
	none, err := parseClientOptions(base)
	if err != nil || none.stateDir != "" {
		t.Fatalf("default directory must stay with clientstate: %+v %v", none, err)
	}
}

func TestCodexSteerFlag(t *testing.T) {
	base := []string{"--broker-url", "http://localhost/mcp?project=test", "--token", "fixture"}
	for _, value := range []string{"", "on", "off", "invalid"} {
		args := append([]string{}, base...)
		if value != "" {
			args = append(args, "--codex-steer", value)
		}
		opts, err := parseClientOptions(args)
		if value == "invalid" {
			if err == nil {
				t.Fatal("invalid steer flag accepted")
			}
			continue
		}
		if err != nil || opts.codexSteerOff != (value == "off") {
			t.Fatal("steer flag default/override", err)
		}
	}
}
