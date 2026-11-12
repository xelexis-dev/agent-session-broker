package serverruntime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/accesslog"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccessLogFlagStartsAndLogsHealth(t *testing.T) {
	dir := t.TempDir()
	output := captureDiagnostics(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []string{"--memory-dir", dir, "--listen", "127.0.0.1:0", "--access-log-file", "logs/access.log"}, "test", io.Discard)
	}()
	bind := ""
	deadline := time.Now().Add(5 * time.Second)
	for bind == "" && time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatal("access log startup rejected")
		default:
		}
		for _, line := range strings.Split(output.String(), "\n") {
			var e struct{ Type, Bind string }
			if json.Unmarshal([]byte(line), &e) == nil && e.Type == "server-listening" {
				bind = e.Bind
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if bind == "" {
		t.Fatal("access log server not ready")
	}
	response, err := http.Get("http://" + bind + "/healthz")
	if err != nil {
		t.Fatal("health failed")
	}
	response.Body.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal("access log shutdown failed")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "logs", "access.log"))
	if err != nil || !strings.Contains(string(raw), `"route":"/healthz"`) {
		t.Fatal("health request was not logged")
	}
}
func TestReleaseTreeExcludesOnlyRootLogDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"logs", "projects/p/logs"} {
		if os.MkdirAll(filepath.Join(dir, name), 0700) != nil {
			t.Fatal("fixture failed")
		}
		if os.WriteFile(filepath.Join(dir, name, "data"), []byte("fixture-log-canary"), 0600) != nil {
			t.Fatal("fixture failed")
		}
	}
	manifest, err := releaseTree(dir)
	if err != nil {
		t.Fatal("backup classification failed")
	}
	if _, ok := manifest["logs/data"]; ok {
		t.Error("reserved logs included in business manifest")
	}
	if _, ok := manifest["projects/p/logs/data"]; !ok {
		t.Error("nested business logs excluded")
	}
}

func TestAccessLogEnvironmentEmptyFlagAndDependencies(t *testing.T) {
	t.Setenv("ASB_ACCESS_LOG_FILE", "logs/from-env.log")
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, []string{"--memory-dir", dir, "--listen", "127.0.0.1:0", "--access-log-file="}, "test", io.Discard); err != nil {
		t.Fatal("explicit empty flag did not override environment")
	}
	if _, err := os.Stat(filepath.Join(dir, "logs")); !os.IsNotExist(err) {
		t.Fatal("explicit off created log files")
	}
	for _, args := range [][]string{{"--access-log-file=", "--access-log-max-files=10"}, {"--access-log-forwarded-for", "--access-log-file="}, {"--access-log-forwarded-for", "--access-log-file=logs/access.log"}, {"--access-log-forwarded-for", "--access-log-file=logs/access.log", "--trusted-proxy=::ffff:10.0.0.1/95"}} {
		if Run(context.Background(), args, "test", io.Discard) == nil {
			t.Fatal("invalid dependency accepted")
		}
	}
	root := t.TempDir()
	if err := Run(ctx, []string{"--memory-dir", root, "--listen", "127.0.0.1:0"}, "test", io.Discard); err != nil {
		t.Fatal("environment opt-in failed")
	}
	if _, err := os.Stat(filepath.Join(root, "logs", "from-env.log")); err != nil {
		t.Fatal("environment file missing")
	}
}
func TestReleaseRestorePreservesRootLogsAndRejectsLogAlias(t *testing.T) {
	source := t.TempDir()
	if os.WriteFile(filepath.Join(source, "business"), []byte("business"), 0600) != nil {
		t.Fatal("fixture failed")
	}
	manifest, err := releaseTree(source)
	if err != nil {
		t.Fatal("fixture failed")
	}
	dest := t.TempDir()
	if os.Mkdir(filepath.Join(dest, "logs"), 0700) != nil || os.WriteFile(filepath.Join(dest, "logs", "keep.log"), []byte("keep"), 0600) != nil {
		t.Fatal("fixture failed")
	}
	if releaseRestore(source, dest, manifest) != nil {
		t.Fatal("normal logs-only target rejected")
	}
	raw, err := os.ReadFile(filepath.Join(dest, "logs", "keep.log"))
	if err != nil || string(raw) != "keep" {
		t.Fatal("existing logs changed on restore")
	}
	bad := t.TempDir()
	if os.WriteFile(filepath.Join(bad, "logs"), nil, 0600) != nil {
		t.Fatal("fixture failed")
	}
	if _, err := releaseTree(bad); err == nil {
		t.Fatal("non-directory reserved logs accepted")
	}
}

func TestReview975CaseInsensitiveReservedLogs(t *testing.T) {
	for _, alias := range []string{"Logs", "LOGS", "loGs"} {
		t.Run(alias, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, alias), 0700); err != nil {
				t.Fatal("alias fixture failed")
			}
			if err := os.WriteFile(filepath.Join(root, alias, "business"), []byte("keep"), 0600); err != nil {
				t.Fatal("alias business fixture failed")
			}
			cfg := accesslog.Defaults()
			cfg.File = "logs/access.log"
			logger, err := accesslog.Open(root, cfg)
			if logger != nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				logger.Close(ctx)
			}
			var init *accesslog.InitError
			if !errors.As(err, &init) || init.Reason != "path_invalid" {
				t.Fatal("nonexact reserved directory was not refused with a fixed class")
			}
			event := ErrorEvent(accessLogFailure(err))
			if event.Reason != "access_log_init_failed" || event.Detail != "path_invalid" {
				t.Fatal("alias startup diagnostic lost its fixed classification")
			}
			manifest, err := releaseTree(root)
			if err != nil || len(manifest) != 1 {
				t.Fatal("alias business directory was excluded or logging created a file")
			}
			if _, ok := manifest[alias+"/business"]; !ok {
				t.Fatal("nonexact directory spelling was excluded from business backup")
			}
			if logger, err := accesslog.Open(root, accesslog.Defaults()); err != nil || logger != nil {
				t.Fatal("disabled logging unexpectedly rejected business directory")
			}
		})
	}
}

func TestReview975RestoreCaseInsensitiveLogs(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "business"), []byte("restore"), 0600); err != nil {
		t.Fatal("source fixture failed")
	}
	manifest, err := releaseTree(source)
	if err != nil {
		t.Fatal("source manifest failed")
	}
	t.Run("alias-rejected", func(t *testing.T) {
		dest := t.TempDir()
		if os.Mkdir(filepath.Join(dest, "Logs"), 0700) != nil || os.WriteFile(filepath.Join(dest, "Logs", "keep"), []byte("keep"), 0600) != nil {
			t.Fatal("alias destination fixture failed")
		}
		cfg := accesslog.Defaults()
		cfg.File = "logs/access.log"
		logger, err := accesslog.Open(dest, cfg)
		if logger != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			logger.Close(ctx)
		}
		if err == nil {
			t.Fatal("alias destination was accepted for access logging")
		}
		if releaseRestore(source, dest, manifest) == nil {
			t.Fatal("nonempty alias business directory was ignored by restore")
		}
		if raw, err := os.ReadFile(filepath.Join(dest, "Logs", "keep")); err != nil || string(raw) != "keep" {
			t.Fatal("alias business directory changed")
		}
	})
	t.Run("exact-logs-preserved", func(t *testing.T) {
		dest := t.TempDir()
		if os.Mkdir(filepath.Join(dest, "logs"), 0700) != nil || os.WriteFile(filepath.Join(dest, "logs", "keep"), []byte("keep"), 0600) != nil {
			t.Fatal("exact directory fixture failed")
		}
		cfg := accesslog.Defaults()
		cfg.File = "logs/access.log"
		logger, err := accesslog.Open(dest, cfg)
		if err != nil {
			t.Fatal("exact logs directory refused")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		logger.Close(ctx)
		if releaseRestore(source, dest, manifest) != nil {
			t.Fatal("restore refused an exact logs-only destination")
		}
		if raw, err := os.ReadFile(filepath.Join(dest, "logs", "keep")); err != nil || string(raw) != "keep" {
			t.Fatal("existing exact logs were not preserved")
		}
		if raw, err := os.ReadFile(filepath.Join(dest, "business")); err != nil || string(raw) != "restore" {
			t.Fatal("business data was not restored")
		}
	})
}
