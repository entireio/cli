package agent_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// envReaders matches every way a package reads the environment. LookupEnv is
// included so the guard cannot be sidestepped by switching call.
var envReaders = regexp.MustCompile(`os\.(Getenv|LookupEnv)\(([^)]*)\)`)

// envReaderOwners are the two non-test files in the agent tree allowed to read
// an arbitrary environment variable, with the reason.
var envReaderOwners = map[string]string{
	"cmd/entire/cli/agent/home.go":           "agent.ResolveHome, the one implementation of relocation-variable policy",
	"cmd/entire/cli/agent/caller_session.go": "reads the caller-session IDs the agents publish, listed in callerSessionEnvVars",
}

// testOverrideConsts name ENTIRE_TEST_* overrides through a constant, so the
// literal prefix check cannot see them.
var testOverrideConsts = map[string]string{
	"piSessionDirEnvVar": "ENTIRE_TEST_PI_SESSION_DIR",
	"cursorChatsDirEnv":  "ENTIRE_TEST_CURSOR_CHATS_DIR",
}

// TestAgentEnvReadsGoThroughResolveHome pins that no agent reads a relocation
// variable on its own. agent.ResolveHome refuses a name missing from
// relocationEnvVars, which catches an agent that calls it with a new variable;
// this guard catches the other shape, an inline os.Getenv("NEW_AGENT_HOME")
// that never reaches the helper, which is how Copilot's COPILOT_HOME read
// looked before the shared resolver existed. Such a read silently escapes the
// static list the test harnesses scrub, and the failure is a test that passes
// on CI and fails on the one contributor who has the variable set.
//
// Allowed: the two owners above, and the ENTIRE_TEST_* overrides each agent
// keeps for its own tests, whether spelled as a literal or through one of the
// constants above.
func TestAgentEnvReadsGoThroughResolveHome(t *testing.T) {
	t.Parallel()
	root, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}
	out := testutil.GitGrepGuard(t, root, "-n", "-E", `os\.(Getenv|LookupEnv)\(`, "--", ":(glob)cmd/entire/cli/agent/**/*.go")

	checked := 0
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		path, rest, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("unparseable git grep line %q", line)
		}
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		checked++
		if _, owner := envReaderOwners[path]; owner {
			continue
		}
		m := envReaders.FindStringSubmatch(rest)
		if m == nil {
			t.Fatalf("%s: matched the grep but not the parser: %q", path, rest)
		}
		arg := strings.TrimSpace(m[2])
		switch {
		case strings.HasPrefix(arg, `"ENTIRE_TEST_`):
			continue
		case testOverrideConsts[arg] != "":
			continue
		}
		t.Errorf("%s reads %s directly; a relocation variable goes through agent.ResolveHome so it is listed in relocationEnvVars, a test override is spelled ENTIRE_TEST_*", path, arg)
	}
	if checked == 0 {
		t.Fatal("no environment reads found under cmd/entire/cli/agent, which cannot be right — the detection pattern has gone stale")
	}
}
