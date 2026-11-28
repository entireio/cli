package testutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// InstallFakeSSH puts an `ssh` first on PATH that answers `ssh -G <host>` the
// way OpenSSH does, with "hostname <aliases[host]>" for a listed alias and
// "hostname <host>" otherwise, and clears GIT_SSH_COMMAND and GIT_SSH so git
// would run plain ssh. It never touches the developer's ~/.ssh/config, which
// OpenSSH reads from the passwd entry rather than $HOME and so cannot be
// redirected per test. Callers should also isolate git config
// (IsolateGitConfigEnv) so a core.sshCommand cannot leak in.
//
// It returns a function listing the hosts `ssh -G` was asked about since
// installation, in call order, for tests that assert ssh was or was not run.
//
// Changes process-global state (PATH): the calling test must not be parallel.
// Skipped on Windows, which cannot run the shell script.
func InstallFakeSSH(t *testing.T, aliases map[string]string) (calls func() []string) {
	t.Helper()
	details := InstallFakeSSHWithCalls(t, aliases)
	return func() []string {
		var hosts []string
		for _, call := range details() {
			hosts = append(hosts, call.Args[len(call.Args)-1])
		}
		return hosts
	}
}

// SSHCall records the full argv and working directory of one fake SSH lookup.
type SSHCall struct {
	Args []string
	Dir  string
}

// InstallFakeSSHWithCalls is InstallFakeSSH with full invocation evidence.
// Supports ssh -G [-l user] [-p port] host. The original helper still returns
// only host names for existing callers.
func InstallFakeSSHWithCalls(t *testing.T, aliases map[string]string) func() []SSHCall {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ssh is a shell script")
	}
	hosts := make([]string, 0, len(aliases))
	for host := range aliases {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	var cases strings.Builder
	for _, host := range hosts {
		cases.WriteString("  " + host + ") echo \"hostname " + aliases[host] + "\" ;;\n")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"[ \"$1\" = \"-G\" ] || exit 255\n" +
		"printf '%s\\0' \"$PWD\" \"$#\" \"$@\" >> '" + logPath + "'\n" +
		"shift\n" +
		"while [ \"$#\" -gt 1 ]; do\n" +
		"  case \"$1\" in -l|-p) [ \"$#\" -ge 3 ] || exit 255; shift 2 ;; *) exit 255 ;; esac\n" +
		"done\n" +
		"echo \"user git\"\n" +
		"case \"$1\" in\n" + cases.String() +
		"  *) echo \"hostname $1\" ;;\n" +
		"esac\n" +
		"echo \"port 22\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil { //nolint:gosec // test helper must be executable
		t.Fatalf("write fake ssh: %v", err)
	}
	// Run it once untimed: the first exec of a freshly written script can be
	// slow (macOS scans new executables), and that cost must not land inside
	// the caller's bounded ssh -G call.
	if out, err := exec.CommandContext(context.Background(), filepath.Join(dir, "ssh"), "-G", "warmup").CombinedOutput(); err != nil {
		t.Fatalf("fake ssh warm-up: %v: %s", err, out)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_SSH_COMMAND", "")
	t.Setenv("GIT_SSH", "")
	if err := os.Remove(logPath); err != nil {
		t.Fatalf("reset fake ssh call log: %v", err)
	}
	return func() []SSHCall {
		data, err := os.ReadFile(logPath) //nolint:gosec // test-owned log
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatalf("read fake ssh calls: %v", err)
		}
		fields := strings.Split(string(data), "\x00")
		var calls []SSHCall
		for len(fields) > 1 {
			n, err := strconv.Atoi(fields[1])
			if err != nil || n < 1 || len(fields) < n+3 {
				t.Fatal("invalid fake ssh call log")
			}
			calls = append(calls, SSHCall{Dir: fields[0], Args: fields[2 : 2+n]})
			fields = fields[2+n:]
		}
		return calls
	}
}
