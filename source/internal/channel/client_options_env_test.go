package channel

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testURL = "https://broker.example/mcp?project=brk"

func writeTokenFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, mode); err != nil {
		t.Fatal(err)
	}
	return file
}

// Core behaviour: the Codex plugin forwards only the broker URL and a token-file path through the environment.
func TestClientOptionsReadTokenFileFromEnvironment(t *testing.T) {
	t.Setenv(envBrokerURL, testURL)
	t.Setenv(envTokenFile, writeTokenFile(t, "asb_p_fixture\n", 0o600))
	opts, err := parseClientOptions(nil)
	if err != nil || opts.token != "asb_p_fixture" || opts.endpoint != testURL || !opts.deviceMode || opts.project != "brk" {
		t.Fatalf("environment options: %+v %v", opts, err)
	}
}

func TestClientOptionsInputPrecedence(t *testing.T) {
	fileToken := writeTokenFile(t, "from-flag-file", 0o600)
	envFileToken := writeTokenFile(t, "from-env-file", 0o600)
	for _, tc := range []struct {
		name  string
		args  []string
		env   map[string]string
		token string
		url   string
	}{
		{"flag beats every other source", []string{"--token", "from-flag", "--token-file", fileToken}, map[string]string{envToken: "from-env", envTokenFile: envFileToken}, "from-flag", testURL},
		{"token variable beats files", []string{"--token-file", fileToken}, map[string]string{envToken: "from-env", envTokenFile: envFileToken}, "from-env", testURL},
		{"file flag beats file variable", []string{"--token-file", fileToken}, map[string]string{envTokenFile: envFileToken}, "from-flag-file", testURL},
		{"file variable alone", nil, map[string]string{envTokenFile: envFileToken}, "from-env-file", testURL},
		{"empty token variable is unset", nil, map[string]string{envToken: "", envTokenFile: envFileToken}, "from-env-file", testURL},
		{"url flag beats url variable", []string{"--broker-url", "https://other.example/mcp?project=p2", "--token", "x1"}, map[string]string{envBrokerURL: testURL}, "x1", "https://other.example/mcp?project=p2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.env[envBrokerURL]; !ok && !strings.Contains(strings.Join(tc.args, " "), "--broker-url") {
				tc.env[envBrokerURL] = testURL
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			opts, err := parseClientOptions(tc.args)
			if err != nil || opts.token != tc.token || opts.endpoint != tc.url {
				t.Fatalf("%+v %v", opts, err)
			}
		})
	}
}

func TestClientOptionsClearEnvironmentAlways(t *testing.T) {
	for _, args := range [][]string{
		{"--broker-url", testURL, "--token", "flag-wins"},
		{"--broker-url", testURL, "--token", "bad value"},
	} {
		t.Setenv(envBrokerURL, "https://ignored.example/mcp?project=x")
		t.Setenv(envToken, "env-secret")
		t.Setenv(envTokenFile, "/nonexistent/token")
		parseClientOptions(args)
		for _, name := range []string{envBrokerURL, envToken, envTokenFile} {
			if _, ok := os.LookupEnv(name); ok {
				t.Fatalf("%s survived parsing of %v", name, args)
			}
		}
	}
}

func TestClientOptionsRequiredInputs(t *testing.T) {
	if _, err := parseClientOptions([]string{"--token", "x1"}); err == nil || err.Error() != "--broker-url or "+envBrokerURL+" is required" {
		t.Fatalf("missing url: %v", err)
	}
	t.Setenv(envToken, "")
	t.Setenv(envTokenFile, "")
	_, err := parseClientOptions([]string{"--broker-url", testURL})
	if err == nil || !strings.Contains(err.Error(), "--token-file") || !strings.Contains(err.Error(), envTokenFile) {
		t.Fatalf("missing token: %v", err)
	}
	// An explicitly empty flag stays an invalid value instead of falling back.
	t.Setenv(envToken, "env-secret")
	if _, err := parseClientOptions([]string{"--broker-url", testURL, "--token", ""}); err == nil {
		t.Fatal("explicit empty --token fell back to the environment")
	}
	if _, err := parseClientOptions([]string{"--broker-url", testURL, "--token-file", ""}); err == nil {
		t.Fatal("explicit empty --token-file accepted")
	}
}

func TestClientOptionsTokenFileContent(t *testing.T) {
	base := []string{"--broker-url", testURL, "--token-file"}
	for _, tc := range []struct {
		content string
		token   string
	}{
		{"asb_p_one\n", "asb_p_one"},
		{"asb_p_two\r\n", "asb_p_two"},
		{"asb_p_three", "asb_p_three"},
	} {
		opts, err := parseClientOptions(append(base, writeTokenFile(t, tc.content, 0o600)))
		if err != nil || opts.token != tc.token {
			t.Fatalf("%q: %+v %v", tc.content, opts, err)
		}
	}
	for _, content := range []string{"", "\n", "asb_p_two_newlines\n\n", "asb p_space\n", "asb_p_x\r", strings.Repeat("x", maxTokenFileBytes+1)} {
		if _, err := parseClientOptions(append(base, writeTokenFile(t, content, 0o600))); err == nil {
			t.Fatalf("accepted token file content of %d bytes", len(content))
		}
	}
}

// Errors may name the path but never the value, the file content or a relative path (it may be a misplaced token).
func TestClientOptionsErrorsNeverEchoValues(t *testing.T) {
	const secret = "asb_p_never_echoed"
	cases := [][]string{
		{"--broker-url", testURL, "--token-file", secret},
		{"--broker-url", testURL, "--token", secret + " x"},
		{"--broker-url", testURL, "--token-file", writeTokenFile(t, secret+" x\n", 0o600)},
		{"--broker-url", testURL, "--token-file", writeTokenFile(t, secret+strings.Repeat("y", maxTokenFileBytes), 0o600)},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, []string{"--broker-url", testURL, "--token-file", writeTokenFile(t, secret, 0o644)})
	}
	for _, args := range cases {
		_, err := parseClientOptions(args)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("error for %d args leaked or missing: %v", len(args), err)
		}
	}
	t.Setenv(envToken, secret+" x")
	if _, err := parseClientOptions([]string{"--broker-url", testURL}); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("environment token error leaked or missing: %v", err)
	}
}

func TestClientOptionsRefuseUnsafeTokenFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := parseClientOptions([]string{"--broker-url", testURL, "--token-file", dir}); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory accepted: %v", err)
	}
	if _, err := parseClientOptions([]string{"--broker-url", testURL, "--token-file", filepath.Join(dir, "missing")}); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := parseClientOptions([]string{"--broker-url", testURL, "--token-file", filepath.Join("relative", "token")}); err == nil || err.Error() != "token file path must be absolute" {
		t.Fatalf("relative path: %v", err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o620, 0o602, 0o610} {
		_, err := parseClientOptions([]string{"--broker-url", testURL, "--token-file", writeTokenFile(t, "asb_p_mode", mode)})
		if runtime.GOOS == "windows" {
			// Windows ACLs are not inspected: a documented limit, not a guarantee.
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "group or other") {
			t.Fatalf("mode %o accepted: %v", mode, err)
		}
	}
}
