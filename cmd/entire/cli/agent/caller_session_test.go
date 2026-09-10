package agent

import (
	"slices"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// callerSessionEnvVarByAgent pins the variable each agent reads. These names
// are not ours to choose — each is set by a third-party binary we do not
// control — so a rename here is a behavioural change that silently stops
// identifying that agent's sessions, with nothing else in the build to notice.
// Established empirically per vendor; do not "fix" an entry without checking
// the agent's own shipped behaviour.
var callerSessionEnvVarByAgent = map[types.AgentName]string{
	AgentNameClaudeCode: "CLAUDE_CODE_SESSION_ID",
	AgentNameCodex:      "CODEX_SESSION_ID",
	AgentNameCursor:     "CURSOR_CONVERSATION_ID",
	AgentNameCopilotCLI: "COPILOT_AGENT_SESSION_ID",
	AgentNamePi:         "PI_SESSION_ID",
}

func TestCallerSessionEnvVar_MatchesTheVendorsName(t *testing.T) {
	for name, want := range callerSessionEnvVarByAgent {
		t.Run(string(name), func(t *testing.T) {
			ag, err := Get(name)
			if err != nil {
				t.Fatalf("Get(%q) error = %v", name, err)
			}
			ident, ok := AsCallerSessionIdentifier(ag)
			if !ok {
				t.Fatalf("%s does not implement CallerSessionIdentifier; it must publish %s", name, want)
			}
			if got := ident.CallerSessionEnvVar(); got != want {
				t.Errorf("CallerSessionEnvVar() = %q, want %q", got, want)
			}
		})
	}
}

// The agents deliberately WITHOUT the capability are as load-bearing as the
// ones with it: Gemini CLI passes its session ID only to its own background
// bookkeeping, and opencode's shell tool augments no environment at all. If
// either gains the capability without an entry above, this fails and asks for
// the mapping to be pinned rather than left implicit.
func TestCallerSessionEnvVar_UnpublishedAgentsStayUnpublished(t *testing.T) {
	for _, name := range List() {
		if _, pinned := callerSessionEnvVarByAgent[name]; pinned {
			continue
		}
		ag, err := Get(name)
		if err != nil {
			continue
		}
		if ident, ok := AsCallerSessionIdentifier(ag); ok {
			t.Errorf("%s newly implements CallerSessionIdentifier (%s) — add it to callerSessionEnvVarByAgent",
				name, ident.CallerSessionEnvVar())
		}
	}
}

func TestCallerSessionEnvVars_ListsEveryPublishingAgent(t *testing.T) {
	got := CallerSessionEnvVars()
	for name, want := range callerSessionEnvVarByAgent {
		if !slices.Contains(got, want) {
			t.Errorf("CallerSessionEnvVars() = %v, missing %s for %s", got, want, name)
		}
	}
}

// The static list and the live registry must agree. This test runs in the one
// package where every agent is registered, which is exactly why the static
// list exists: elsewhere the registry enumeration silently returns a subset
// (the e2e harness links eight of nine agents), and a name missing from a
// test's isolation set leaks the developer's own session into the run.
func TestCallerSessionEnvVars_MatchesTheRegistry(t *testing.T) {
	fromRegistry := callerSessionEnvVarsFromRegistry()
	slices.Sort(fromRegistry)
	static := CallerSessionEnvVars()
	slices.Sort(static)
	if !slices.Equal(static, fromRegistry) {
		t.Errorf("callerSessionEnvVars = %v, registry enumerates %v — update the static list", static, fromRegistry)
	}
}

// CallerSessionEnvVars must not hand out its backing array: a caller that
// sorts or truncates the result would corrupt the list for everyone else in
// the process.
func TestCallerSessionEnvVars_ReturnsACopy(t *testing.T) {
	first := CallerSessionEnvVars()
	if len(first) == 0 {
		t.Fatal("CallerSessionEnvVars() is empty")
	}
	first[0] = "MUTATED"
	if CallerSessionEnvVars()[0] == "MUTATED" {
		t.Error("CallerSessionEnvVars() shares its backing array with the package-level list")
	}
}

// clearCallerSessionEnv unsets every agent's caller-session variable.
//
// `go test` is routinely run from inside one of these agents — this suite's
// own development happened inside Claude Code, which sets
// CLAUDE_CODE_SESSION_ID — so without this a test asserting "no candidates"
// passes on CI and fails on a contributor's machine. Derived from
// CallerSessionEnvVars() rather than a hand-written list so it cannot rot when
// an agent is added.
func clearCallerSessionEnv(t *testing.T) {
	t.Helper()
	for _, name := range CallerSessionEnvVars() {
		t.Setenv(name, "")
	}
}

func TestCallerSessionCandidates_ReadsThePublishedID(t *testing.T) {
	clearCallerSessionEnv(t)
	t.Setenv("CODEX_SESSION_ID", "01a0800c-91dd-7483-ba76-1df61bb5dd4f")

	got := CallerSessionCandidates()
	if len(got) != 1 {
		t.Fatalf("CallerSessionCandidates() = %+v, want exactly 1", got)
	}
	if got[0].SessionID != "01a0800c-91dd-7483-ba76-1df61bb5dd4f" {
		t.Errorf("SessionID = %q, want the value of CODEX_SESSION_ID", got[0].SessionID)
	}
	if got[0].AgentType != AgentTypeCodex {
		t.Errorf("AgentType = %q, want %q", got[0].AgentType, AgentTypeCodex)
	}
}

func TestCallerSessionCandidates_NoneWhenNothingPublished(t *testing.T) {
	clearCallerSessionEnv(t)
	if got := CallerSessionCandidates(); len(got) != 0 {
		t.Errorf("CallerSessionCandidates() = %+v, want none", got)
	}
}

// Nesting is the normal multi-candidate case, not a conflict: a codex run
// started from Claude Code's shell tool sees both variables. Both are
// reported, because only process ancestry can say which is nearer and this
// layer has no session state to match against.
func TestCallerSessionCandidates_ReportsEveryNestedClaim(t *testing.T) {
	clearCallerSessionEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "outer-session")
	t.Setenv("CODEX_SESSION_ID", "inner-session")

	got := CallerSessionCandidates()
	if len(got) != 2 {
		t.Fatalf("CallerSessionCandidates() = %+v, want both claims", got)
	}
	ids := []string{got[0].SessionID, got[1].SessionID}
	for _, want := range []string{"outer-session", "inner-session"} {
		if !slices.Contains(ids, want) {
			t.Errorf("CallerSessionCandidates() ids = %v, missing %q", ids, want)
		}
	}
}

// An ID out of the environment becomes a path component in
// ResolveSessionFile, so a value that is not path-safe must read as absent
// rather than be passed along. Reporting absent degrades to a weaker
// resolution tier, which is the same outcome as the variable never being set.
func TestCallerSessionCandidates_RejectsUnsafeIDs(t *testing.T) {
	for _, id := range []string{
		"../../../etc/passwd",
		"a/b",
		"-rf",
		"has space",
		"   ",
		"semi;colon",
	} {
		t.Run(id, func(t *testing.T) {
			clearCallerSessionEnv(t)
			t.Setenv("PI_SESSION_ID", id)
			if got := CallerSessionCandidates(); len(got) != 0 {
				t.Errorf("CallerSessionCandidates() = %+v for PI_SESSION_ID=%q, want none", got, id)
			}
		})
	}
}

func TestCallerSessionCandidates_TrimsSurroundingWhitespace(t *testing.T) {
	clearCallerSessionEnv(t)
	t.Setenv("PI_SESSION_ID", "  01a08012-ce85-7bc0-8e05-fe0047687581\n")

	got := CallerSessionCandidates()
	if len(got) != 1 {
		t.Fatalf("CallerSessionCandidates() = %+v, want exactly 1", got)
	}
	if got[0].SessionID != "01a08012-ce85-7bc0-8e05-fe0047687581" {
		t.Errorf("SessionID = %q, want it trimmed", got[0].SessionID)
	}
}
