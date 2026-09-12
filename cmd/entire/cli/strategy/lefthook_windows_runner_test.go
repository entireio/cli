package strategy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLefthookScriptRunsUnderItsRunnerOnWindows exercises the one assumption
// in the Lefthook integration that does not hold by construction.
//
// Entire's hooks reach Lefthook as `scripts` declared with `runner: bash`, so
// Lefthook resolves bash through PATH and spawns it with the script. Every
// other artifact is a file Entire writes and reads itself, but this step is
// Lefthook executing a POSIX script on a platform whose shell is not POSIX —
// and if bash does not resolve, or the script does not run under it, Entire
// silently stops capturing in every Lefthook repo on Windows.
//
// The name carries "Windows" because that is how ci.yml selects the tests the
// windows-latest job runs. It runs everywhere, which is what makes a failure
// on Windows alone meaningful rather than a missing harness.
//
// Scope worth stating: passing proves bash is resolvable and the script runs
// under it on the machine running the test. A GitHub Windows runner ships Git
// for Windows with bash on PATH; a developer who installed Git with the
// "from cmd only" option may not, and this test cannot speak for that machine.
func TestLefthookScriptRunsUnderItsRunnerOnWindows(t *testing.T) {
	t.Parallel()

	bash, err := exec.LookPath("bash")
	require.NoErrorf(t, err, "lefthook spawns the declared runner through PATH, "+
		"so no bash on %s means Entire's hooks never run in a Lefthook repo there", runtime.GOOS)

	dir := t.TempDir()

	// A stand-in for the entire binary, so the script's `command -v entire`
	// guard is satisfied and the arguments it forwards can be observed.
	// ToSlash because the path is interpolated into a shell script: bash keeps
	// backslashes literal inside single quotes, so a Windows path would be a
	// mangled filename rather than a directory walk.
	record := filepath.Join(dir, "args.txt")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(filepath.ToSlash(record)) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "entire"), []byte(stub), 0o755))

	var prePush hookSpec
	for _, spec := range buildHookSpecs("entire") {
		if spec.name == "pre-push" {
			prePush = spec
		}
	}
	require.NotEmpty(t, prePush.name, "pre-push spec")
	script := filepath.Join(dir, lefthookScript)
	require.NoError(t, os.WriteFile(script, []byte(renderLefthookScript(prePush)), 0o755))

	// Exactly what Lefthook does: the runner, the script, and the hook's own
	// arguments — pre-push gets the remote and its URL.
	cmd := exec.CommandContext(t.Context(), bash, lefthookScript, "origin", "https://example.com/x.git")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "running Entire's Lefthook script under bash: %s", out)

	got, err := os.ReadFile(record)
	require.NoError(t, err, "the script did not reach the entire binary: %s", out)
	require.Equal(t, "hooks git pre-push origin", strings.TrimSpace(string(got)),
		"the hook's arguments must survive the runner")
}

// requireLefthookEnv names the variable CI sets so a lefthook that failed to
// install is a failure rather than a silent skip.
const requireLefthookEnv = "ENTIRE_TEST_REQUIRE_LEFTHOOK"

// TestLefthookDeliversEntireOnWindows is the whole chain with the real
// Lefthook binary: git runs Lefthook's generated hook, Lefthook resolves
// Entire's config through `extends`, and spawns Entire's script with the
// hook's arguments. Nothing here is stubbed except the entire binary itself.
//
// It carries "Windows" in its name so ci.yml's windows-latest job selects it;
// that job installs lefthook and sets ENTIRE_TEST_REQUIRE_LEFTHOOK, so a
// missing binary there fails instead of skipping. Elsewhere it runs whenever
// lefthook happens to be on PATH.
func TestLefthookDeliversEntireOnWindows(t *testing.T) {
	lefthook, err := exec.LookPath("lefthook")
	if err != nil {
		if os.Getenv(requireLefthookEnv) != "" {
			t.Fatalf("%s is set but lefthook is not on PATH: %v", requireLefthookEnv, err)
		}
		t.Skip("lefthook not installed")
	}

	dir := newLefthookRepo(t, "")
	binDir := filepath.Join(t.TempDir(), "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	record := filepath.Join(binDir, "args.txt")
	// hookCmdPrefix resolves to a bare "entire", so a stub on PATH stands in.
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "entire"),
		[]byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+shellQuote(filepath.ToSlash(record))+"\n"), 0o755))

	env := append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), name, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "%s %v: %s", name, args, out)
	}

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi\n"), 0o644))
	run("git", "add", "a.txt")
	run("git", "commit", "-m", "init", "--no-verify")

	_, err = EnsureLefthookIntegration(t.Context())
	require.NoError(t, err)
	// Lefthook only generates a hook file per hook it knows about, and it
	// learns Entire's from the extends entry written above.
	run(lefthook, "install")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi\nthere\n"), 0o644))
	run("git", "add", "a.txt")
	run("git", "commit", "-m", "captured")

	got, err := os.ReadFile(record)
	require.NoError(t, err, "Lefthook never reached Entire")
	lines := strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(got)), "\n", " "))
	for _, hook := range []string{"prepare-commit-msg", "commit-msg", "post-commit"} {
		require.Containsf(t, lines, hook, "Lefthook did not dispatch %s to Entire: %q", hook, got)
	}
}
