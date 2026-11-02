package githelper

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// transport-helper.c C-quotes string option values. Whether non-ASCII is
// escaped depends on core.quotePath, so both settings run. With false,
// non-ASCII arrives raw and would pass even without unquoting.
var quotePathSettings = []string{"true", "false"}

func gitPushVia(t *testing.T, dir, path string, protocolVersion int, quotePath string, args ...string) (string, error) {
	t.Helper()
	full := append([]string{
		"-c", "protocol.version=" + strconv.Itoa(protocolVersion),
		"-c", "core.quotePath=" + quotePath,
		"push", "origin",
	}, args...)
	cmd := exec.CommandContext(t.Context(), "git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestGitPush_ForceWithLease_QuotedRefNames(t *testing.T) {
	// No t.Parallel(): newRealGitRepo sets process env.
	if runtime.GOOS == goosWindows {
		t.Skip("shell-script helper stub is POSIX-only")
	}
	branches := []string{"café", `feat/"quoted"`, "日本/ブランチ"}

	for _, protocolVersion := range []int{2, 0} {
		for _, quotePath := range quotePathSettings {
			t.Run("protocol v"+strconv.Itoa(protocolVersion)+" quotePath="+quotePath, func(t *testing.T) {
				server, bare := gitBackendServer(t)
				dir, run := newRealGitRepo(t)
				const scheme = "entiretest"
				path := helperStub(t, scheme, server.URL, protocolVersion)
				run("remote", "add", "origin", scheme+"://host/et/owner/repo")

				for _, branch := range branches {
					t.Run(branch, func(t *testing.T) {
						ref := "refs/heads/" + branch
						run("commit", "-q", "--allow-empty", "-m", branch)
						served := run("rev-parse", "HEAD")
						run("push", "-q", bare, served+":"+ref)
						run("commit", "-q", "--amend", "--allow-empty", "-m", branch+", rewritten")
						rewritten := run("rev-parse", "HEAD")
						tip := func() string { return runGit(t, "-C", bare, "rev-parse", ref) }

						out, err := gitPushVia(t, dir, path, protocolVersion, quotePath,
							"--force-with-lease="+ref+":"+rewritten, rewritten+":"+ref)
						if err == nil {
							t.Fatalf("stale-lease push succeeded:\n%s", out)
						}
						if !strings.Contains(out, "stale info") {
							t.Errorf("output lacks stale info rejection:\n%s", out)
						}
						if got := tip(); got != served {
							t.Fatalf("served tip moved to %s, want %s", got, served)
						}

						out, err = gitPushVia(t, dir, path, protocolVersion, quotePath,
							"--force-with-lease="+ref+":"+served, rewritten+":"+ref)
						if err != nil {
							t.Fatalf("matching-lease push: %v\n%s", err, out)
						}
						if got := tip(); got != rewritten {
							t.Errorf("served tip = %s, want rewrite %s\n%s", got, rewritten, out)
						}
					})
				}
			})
		}
	}
}

func TestGitPush_PushOptionQuoting(t *testing.T) {
	// No t.Parallel(): newRealGitRepo sets process env.
	if runtime.GOOS == goosWindows {
		t.Skip("shell-script helper stub is POSIX-only")
	}
	values := []string{"plain", "café", `say "hi"`, `back\slash`, "日本"}

	for _, protocolVersion := range []int{2, 0} {
		for _, quotePath := range quotePathSettings {
			t.Run("protocol v"+strconv.Itoa(protocolVersion)+" quotePath="+quotePath, func(t *testing.T) {
				server, bare := gitBackendServer(t)
				runGit(t, "-C", bare, "config", "receive.advertisePushOptions", "true")
				received := filepath.Join(t.TempDir(), "push-options")
				hooks := t.TempDir()
				hook := "#!/bin/sh\n" +
					"i=0\n" +
					"while [ \"$i\" -lt \"${GIT_PUSH_OPTION_COUNT:-0}\" ]; do\n" +
					"  eval \"printf '%s\\n' \\\"\\$GIT_PUSH_OPTION_$i\\\"\" >> '" + received + "'\n" +
					"  i=$((i + 1))\n" +
					"done\n"
				if err := os.WriteFile(filepath.Join(hooks, "pre-receive"), []byte(hook), 0o755); err != nil {
					t.Fatal(err)
				}
				runGit(t, "-C", bare, "config", "core.hooksPath", hooks)

				dir, run := newRealGitRepo(t)
				const scheme = "entiretest"
				path := helperStub(t, scheme, server.URL, protocolVersion)
				run("remote", "add", "origin", scheme+"://host/et/owner/repo")
				run("commit", "-q", "--allow-empty", "-m", "one")

				args := make([]string, 0, len(values)+1)
				for _, v := range values {
					args = append(args, "--push-option="+v)
				}
				args = append(args, testRefMain+":"+testRefMain)
				if out, err := gitPushVia(t, dir, path, protocolVersion, quotePath, args...); err != nil {
					t.Fatalf("git push: %v\n%s", err, out)
				}

				data, err := os.ReadFile(received)
				if err != nil {
					t.Fatalf("pre-receive did not record push options: %v", err)
				}
				got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
				if strings.Join(got, "\x00") != strings.Join(values, "\x00") {
					t.Errorf("server received push options %q, want %q", got, values)
				}
			})
		}
	}
}

// cQuote mimics quote_c_style with core.quotePath=true.
func cQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(s) {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c >= 0x7f:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// git checks a stale lease client-side before sending `option cas`, so
// TestGitPush_ForceWithLease_QuotedRefNames never reaches the helper
// with one. This drives quoted leases through Set into send-pack.
func TestHandlePush_QuotedLease(t *testing.T) {
	// No t.Parallel(): t.Chdir for send-pack cwd.
	server, bare := gitBackendServer(t)
	dir, run := newRealGitRepo(t)
	t.Chdir(dir)

	for _, branch := range []string{"café", `feat/"quoted"`, "日本/ブランチ"} {
		t.Run(branch, func(t *testing.T) {
			ref := "refs/heads/" + branch
			run("commit", "-q", "--allow-empty", "-m", branch)
			served := run("rev-parse", "HEAD")
			run("push", "-q", bare, served+":"+ref)
			run("commit", "-q", "--amend", "--allow-empty", "-m", branch+", rewritten")
			rewritten := run("rev-parse", "HEAD")

			push := func(t *testing.T, lease string) (string, error) {
				t.Helper()
				opts := &Options{}
				if reply := opts.Set("cas", cQuote(ref+":"+lease)); reply != "ok" {
					t.Fatalf("Set(cas) = %q, want ok", reply)
				}
				var stdout bytes.Buffer
				err := handlePush(t.Context(), testTransport(server), &refAdvCache{},
					"push "+rewritten+":"+ref, opts,
					bufio.NewReader(strings.NewReader("\n")), &stdout)
				return stdout.String(), err
			}
			tip := func() string { return runGit(t, "-C", bare, "rev-parse", ref) }

			out, err := push(t, rewritten)
			if err == nil {
				t.Fatalf("stale-lease push succeeded: %q", out)
			}
			if !strings.Contains(out, "stale info") {
				t.Errorf("stdout = %q, want stale info error", out)
			}
			if got := tip(); got != served {
				t.Fatalf("served tip moved to %s, want %s", got, served)
			}

			out, err = push(t, served)
			if err != nil {
				t.Fatalf("handlePush: %v\nstdout: %s", err, out)
			}
			if got := tip(); got != rewritten {
				t.Errorf("served tip = %s, want rewrite %s", got, rewritten)
			}
		})
	}
}

// transport-helper ignores the reply to `option cas` and still sends the
// forced refspec, so a rejected lease must stop the push in the helper.
func TestHandlePush_MalformedLeaseRefusesPush(t *testing.T) {
	// No t.Parallel(): t.Chdir for send-pack cwd.
	server, bare := gitBackendServer(t)
	dir, run := newRealGitRepo(t)
	t.Chdir(dir)

	const ref = "refs/heads/leased"
	run("commit", "-q", "--allow-empty", "-m", "served")
	served := run("rev-parse", "HEAD")
	run("push", "-q", bare, served+":"+ref)
	run("commit", "-q", "--amend", "--allow-empty", "-m", "rewritten")
	rewritten := run("rev-parse", "HEAD")

	opts := &Options{}
	if reply := opts.Set("cas", `"`+ref+`:`+served); reply != optionReplyInvalid {
		t.Fatalf("Set(cas) = %q, want %q", reply, optionReplyInvalid)
	}
	var stdout bytes.Buffer
	err := handlePush(t.Context(), testTransport(server), &refAdvCache{},
		"push +"+rewritten+":"+ref, opts,
		bufio.NewReader(strings.NewReader("\n")), &stdout)
	if err == nil {
		t.Fatalf("push with rejected lease succeeded: %q", stdout.String())
	}
	if got := runGit(t, "-C", bare, "rev-parse", ref); got != served {
		t.Fatalf("served tip moved to %s, want %s", got, served)
	}
}
