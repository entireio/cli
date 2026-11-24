package githelper

import (
	"bufio"
	"bytes"
	"context"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
)

// Git sends leases to helpers as `option cas`.

// IsolateRepository first: http-backend inherits developer config.
func gitBackendServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	gitenv.IsolateRepository(t)
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "et", "owner", "repo")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, "init", "-q", "--bare", "-b", "main", bare)
	runGit(t, "-C", bare, "config", "http.receivepack", "true")

	env := []string{
		"GIT_PROJECT_ROOT=" + root,
		"GIT_HTTP_EXPORT_ALL=1",
	}
	// cgi.Handler drops GIT_CONFIG_* unless copied.
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GIT_CONFIG_") {
			env = append(env, e)
		}
	}
	server := httptest.NewServer(&cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env:  env,
	})
	t.Cleanup(server.Close)
	return server, bare
}

func runGit(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func seedDivergedBranch(t *testing.T, bare string, run func(args ...string) string) (served, rewritten string) {
	t.Helper()
	run("commit", "-q", "--allow-empty", "-m", "one")
	served = run("rev-parse", "HEAD")
	run("push", "-q", bare, testRefMain+":"+testRefMain)
	run("commit", "-q", "--amend", "--allow-empty", "-m", "one, rewritten")
	rewritten = run("rev-parse", "HEAD")
	if served == rewritten {
		t.Fatal("amend did not rewrite the commit")
	}
	return served, rewritten
}

func bareTip(t *testing.T, bare string) string {
	t.Helper()
	return runGit(t, "-C", bare, "rev-parse", testRefMain)
}

func TestHandlePush_ForceWithLease(t *testing.T) {
	// No t.Parallel(): t.Chdir for send-pack cwd.
	server, bare := gitBackendServer(t)
	dir, run := newRealGitRepo(t)
	served, rewritten := seedDivergedBranch(t, bare, run)
	t.Chdir(dir)

	push := func(t *testing.T, opts *Options) (string, error) {
		t.Helper()
		var stdout bytes.Buffer
		err := handlePush(t.Context(), testTransport(server), &refAdvCache{},
			"push "+testRefMain+":"+testRefMain, opts,
			bufio.NewReader(strings.NewReader("\n")), &stdout)
		return stdout.String(), err
	}

	t.Run("no lease is rejected", func(t *testing.T) {
		out, err := push(t, &Options{})
		if err == nil {
			t.Fatalf("non-fast-forward push without force succeeded: %q", out)
		}
		if !strings.Contains(out, "error "+testRefMain+" non-fast forward") {
			t.Errorf("stdout = %q, want non-fast-forward error", out)
		}
		if got := bareTip(t, bare); got != served {
			t.Errorf("served tip moved to %s, want %s", got, served)
		}
	})

	t.Run("stale lease is rejected", func(t *testing.T) {
		out, err := push(t, &Options{cas: []string{testRefMain + ":" + rewritten}})
		if err == nil {
			t.Fatalf("stale-lease push succeeded: %q", out)
		}
		if !strings.Contains(out, "error "+testRefMain+" stale info") {
			t.Errorf("stdout = %q, want stale info error", out)
		}
		if got := bareTip(t, bare); got != served {
			t.Errorf("served tip moved to %s, want %s", got, served)
		}
	})

	t.Run("exact lease lands", func(t *testing.T) {
		out, err := push(t, &Options{cas: []string{testRefMain + ":" + served}})
		if err != nil {
			t.Fatalf("handlePush: %v\nstdout: %s", err, out)
		}
		if !strings.Contains(out, "ok "+testRefMain) {
			t.Errorf("stdout = %q, want ok", out)
		}
		if got := bareTip(t, bare); got != rewritten {
			t.Errorf("served tip = %s, want rewrite %s", got, rewritten)
		}
	})
}

// Re-executes this test binary as the helper.
func helperStub(t *testing.T, scheme, serverURL string, protocolVersion int) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stubDir := t.TempDir()
	script := "#!/bin/sh\n" +
		helperServerEnv + "='" + serverURL + "' " +
		helperProtocolEnv + "=" + strconv.Itoa(protocolVersion) + " " +
		"exec '" + exe + "'\n"
	if err := os.WriteFile(filepath.Join(stubDir, "git-remote-"+scheme), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stubDir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// v2 uses helper push; v0 uses connect.
func TestGitPush_ForceWithLease(t *testing.T) {
	// No t.Parallel(): newRealGitRepo sets process env.
	if runtime.GOOS == goosWindows {
		t.Skip("shell-script helper stub is POSIX-only")
	}
	for _, protocolVersion := range []int{2, 0} {
		t.Run("protocol v"+strconv.Itoa(protocolVersion), func(t *testing.T) {
			server, bare := gitBackendServer(t)
			dir, run := newRealGitRepo(t)
			served, rewritten := seedDivergedBranch(t, bare, run)
			const scheme = "entiretest"
			path := helperStub(t, scheme, server.URL, protocolVersion)
			run("remote", "add", "origin", scheme+"://host/et/owner/repo")
			// Bare --force-with-lease reads this tracking ref.
			run("update-ref", "refs/remotes/origin/main", served)

			gitPush := func(t *testing.T, args ...string) (string, error) {
				t.Helper()
				full := append([]string{"-c", "protocol.version=" + strconv.Itoa(protocolVersion), "push", "origin"}, args...)
				cmd := exec.CommandContext(t.Context(), "git", full...)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "PATH="+path)
				out, err := cmd.CombinedOutput()
				return string(out), err
			}

			t.Run("no lease is rejected", func(t *testing.T) {
				out, err := gitPush(t, testRefMain+":"+testRefMain)
				if err == nil {
					t.Fatalf("non-fast-forward push succeeded:\n%s", out)
				}
				if !strings.Contains(out, "[rejected]") || !strings.Contains(out, "non-fast-forward") {
					t.Errorf("output lacks non-fast-forward rejection:\n%s", out)
				}
				if got := bareTip(t, bare); got != served {
					t.Errorf("served tip moved to %s, want %s", got, served)
				}
			})

			t.Run("stale lease is rejected", func(t *testing.T) {
				out, err := gitPush(t, "--force-with-lease="+testRefMain+":"+rewritten, testRefMain+":"+testRefMain)
				if err == nil {
					t.Fatalf("stale-lease push succeeded:\n%s", out)
				}
				if !strings.Contains(out, "stale info") {
					t.Errorf("output lacks stale info rejection:\n%s", out)
				}
				if got := bareTip(t, bare); got != served {
					t.Errorf("served tip moved to %s, want %s", got, served)
				}
			})

			t.Run("exact lease lands", func(t *testing.T) {
				out, err := gitPush(t, "--force-with-lease="+testRefMain+":"+served, testRefMain+":"+testRefMain)
				if err != nil {
					t.Fatalf("git push --force-with-lease: %v\n%s", err, out)
				}
				if got := bareTip(t, bare); got != rewritten {
					t.Errorf("served tip = %s, want rewrite %s\n%s", got, rewritten, out)
				}
				// Push moved the tracking ref too; rewind.
				runGit(t, "-C", bare, "update-ref", testRefMain, served)
				run("update-ref", "refs/remotes/origin/main", served)
			})

			t.Run("tracking-ref lease lands", func(t *testing.T) {
				out, err := gitPush(t, "--force-with-lease", testRefMain+":"+testRefMain)
				if err != nil {
					t.Fatalf("git push --force-with-lease: %v\n%s", err, out)
				}
				if got := bareTip(t, bare); got != rewritten {
					t.Errorf("served tip = %s, want rewrite %s\n%s", got, rewritten, out)
				}
			})
		})
	}
}
