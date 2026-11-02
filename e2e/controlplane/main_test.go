//go:build e2e

// Package controlplane drives the entire binary against the production
// control plane as a real GitHub test user. TestMain logs in once for the
// whole package; every test starts from that session.
package controlplane

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/entireio/cli/e2e/entire"
	"github.com/entireio/cli/e2e/testutil"
)

func TestMain(m *testing.M) {
	username, password, totpSecret := os.Getenv("E2E_GH_USERNAME"), os.Getenv("E2E_GH_PASSWORD"), os.Getenv("E2E_GH_TOTP_SECRET")
	if username == "" || password == "" || totpSecret == "" {
		fmt.Fprintln(os.Stderr, "preflight: E2E_GH_USERNAME, E2E_GH_PASSWORD, and E2E_GH_TOTP_SECRET must be set (GitHub test user for `entire login --device`)")
		os.Exit(1)
	}
	// Nothing spawned below (entire, git, git-remote-entire, Chromium) needs
	// the GitHub credentials.
	os.Unsetenv("E2E_GH_USERNAME")
	os.Unsetenv("E2E_GH_PASSWORD")
	os.Unsetenv("E2E_GH_TOTP_SECRET")

	runDir := os.Getenv("E2E_ARTIFACT_DIR")
	if runDir == "" {
		_, file, _, _ := runtime.Caller(0)
		testutil.ArtifactRoot = filepath.Join(filepath.Dir(file), "..", "artifacts")
		runDir = testutil.ArtifactRunDir()
	}
	_ = os.MkdirAll(runDir, 0o755)
	testutil.SetRunDir(runDir)

	// The login below stores a real refresh token, and CI uploads
	// e2e/artifacts/ on every run, so the CLI's config, cache, and token store
	// live in a temp dir outside the artifact tree. testing.Testing() is false
	// in the spawned binary, so only these env vars keep it away from the
	// developer's real ~/.config/entire and OS keychain.
	stateDir, err := os.MkdirTemp("", "e2e-controlplane-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "preflight: create state dir: %v\n", err)
		os.Exit(1)
	}
	// Playwright derives its browser directory from XDG_CACHE_HOME on Linux,
	// so pin it to the real user cache before that variable is redirected.
	if os.Getenv("PLAYWRIGHT_BROWSERS_PATH") == "" {
		userCache, err := os.UserCacheDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "preflight: user cache dir: %v\n", err)
			os.Exit(1)
		}
		os.Setenv("PLAYWRIGHT_BROWSERS_PATH", filepath.Join(userCache, "ms-playwright"))
	}
	os.Setenv("ENTIRE_TOKEN_STORE", "file")
	os.Setenv("ENTIRE_TOKEN_STORE_PATH", filepath.Join(stateDir, "tokens.json"))
	os.Setenv("ENTIRE_CONFIG_DIR", filepath.Join(stateDir, "entire-config"))
	os.Setenv("XDG_CACHE_HOME", filepath.Join(stateDir, "entire-cache"))
	// ENTIRE_TOKEN's presence alone switches the CLI to token mode, and a set
	// ENTIRE_AUTH_BASE_URL (retired) fails every command.
	for _, name := range []string{"ENTIRE_TOKEN", "ENTIRE_CONTEXT", "ENTIRE_AUTH_BASE_URL"} {
		os.Unsetenv(name)
	}

	// git-remote-entire is built beside entire by `mise run build`; git
	// resolves it from PATH when cloning an entire:// URL.
	entireBin := entire.BinPath()
	os.Setenv("PATH", filepath.Dir(entireBin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, bin := range []string{"git", "git-remote-entire"} {
		if _, err := exec.LookPath(bin); err != nil {
			fmt.Fprintf(os.Stderr, "preflight: %s not found on PATH\n", bin)
			os.Exit(1)
		}
	}

	version := "unknown"
	if out, err := exec.Command(entireBin, "version").Output(); err == nil {
		version = string(out)
	}
	// gotestsum swallows stdout and stderr, so the mise task cats this file.
	preflight := fmt.Sprintf("entire binary:  %s\nentire version: %s\n", entireBin, version)
	_ = os.WriteFile(filepath.Join(runDir, "entire-version.txt"), []byte(preflight), 0o644)

	gitenv.IsolateMain()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	err = deviceLogin(ctx, stateDir, username, password, totpSecret)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "control-plane e2e: login failed: %v\n", err)
		_ = os.RemoveAll(stateDir)
		os.Exit(1)
	}

	code := m.Run()
	// Revoke the session on the control plane. Deleting the token file alone
	// leaves a live login on the shared account after every run.
	logoutCtx, cancelLogout := context.WithTimeout(context.Background(), 30*time.Second)
	if out, err := execx.NonInteractive(logoutCtx, entireBin, "logout").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "control-plane e2e: logout failed: %v\n%s", err, out)
	}
	cancelLogout()
	_ = os.RemoveAll(stateDir)
	os.Exit(code)
}
