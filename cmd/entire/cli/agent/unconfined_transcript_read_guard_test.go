package agent_test

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// unconfinedTranscriptReadPattern matches qualified, aliased, and unqualified
// calls to the legacy reader, excluding its UnderHome sibling. Adopted paths
// need confinement at read time, even after validation in another process.
// Raw os.ReadFile/os.Open calls have a separate transcript-read guard.
const unconfinedTranscriptReadPattern = `\bReadTranscriptFile\(`

// unconfinedTranscriptFileReads lists every non-test call to
// agent.ReadTranscriptFile the guard currently finds, one reason per call, in
// the order git grep reports them within each file. The count of reasons for
// a file must equal the count of matches in it — both directions are
// enforced below, so a new call must be justified here before it can land,
// and a reason cannot quietly outlive the call it describes.
//
// A reason must say why THIS read cannot receive a path sourced from adopted
// session.State (the recorded AgentHome mechanism session_adopt.go trusts) —
// not merely that nobody got around to confining it. The common, legitimate
// shapes seen below:
//   - the parameter is a live hook event's self-reported path (sessionRef /
//     transcriptRef / event.SessionRef), supplied by the agent process
//     invoking this same hook, same turn — a different trust boundary than
//     adopted state;
//   - the owning agent does not implement AgentHomeProvider, so
//     state.AgentHome is structurally always "" for it
//     (strategy.resolveAgentHome's !ok branch) — nothing to confine against;
//   - the call site is dead code today, reachable only from tests or an
//     unreachable production path, carrying its own SECURITY comment
//     describing the confinement a future caller MUST add; or
//   - every adopted-state-reachable caller of this method now prefers a
//     Confined*/UnderHome sibling instead, so this bare path only survives
//     for callers outside that threat class.
var unconfinedTranscriptFileReads = map[string][]string{
	"cmd/entire/cli/agent/pi/transcript.go": {
		"GetTranscriptPosition's own path parameter. Every adopted-state-reachable caller " +
			"(hasNewTranscriptWork, advanceCheckpointTranscriptStartToTurnEnd, the deferred " +
			"turn-end offset advance in manual_commit_condensation.go) now calls " +
			"agent.GetTranscriptPositionUnderHome first, which never falls through to this " +
			"method for Pi since Pi implements ConfinedTranscriptAnalyzer; the only remaining " +
			"caller is state.go's CapturePrePromptState (sessionRef, the live hook event's " +
			"self-reported path).",
		"ExtractModifiedFilesFromOffset's own path parameter. Same disposition as Codex's " +
			"ExtractModifiedFilesFromOffset above: reachable via both live-hook paths " +
			"(lifecycle.go:887 and :1853, see the claudecode entry for why :1853 is " +
			"event-sourced too); the manual_commit_hooks.go else branch is never taken for Pi " +
			"(ConfinedTranscriptAnalyzer wins the dispatch there first).",
		"ExtractPrompts's own sessionRef parameter. Reachable only from attach.go (explicit " +
			"user-supplied CLI path) and a live hook handler; condensation's late-flush path — " +
			"the one adopted-state-reachable caller — prefers ExtractPromptsFromTranscript via " +
			"the TranscriptPromptExtractor branch instead.",
	},
	"cmd/entire/cli/agent/pi/pi.go": {
		"ReadTranscript's own sessionRef parameter. Its only callers in the whole repo are " +
			"lifecycle.go:806 (transcriptRef, live hook) and attach.go:272 (explicit " +
			"user-supplied CLI path) — never an adopted session.State path.",
		"ReadSession: dead code, see the SECURITY comment on this method. Its only caller in " +
			"the whole repo is agent/external/capabilities.go's generic passthrough wrapper, " +
			"invoked only for external agents, which can never resolve to a *PiAgent.",
	},
	"cmd/entire/cli/agent/pi/lifecycle.go": {
		"extractModelFromPiSessionFile's path parameter. Its only caller (lifecycle.go:210) " +
			"passes sessionRef, the live hook payload's self-reported path for the agent " +
			"process invoking this same hook — never an adopted session.State path.",
	},
	"cmd/entire/cli/agent/opencode/opencode.go": {
		"OpenCode has no AgentHomeProvider. Its transcript cache is confined through .entire; " +
			"adoption's fallback clears the irrelevant AgentHome field.",

		"ReadSession's own input.SessionRef. Same reason as ReadTranscript above.",
	},
	"cmd/entire/cli/agent/opencode/transcript.go": {
		"parseExportSessionFromFile's path parameter (feeds ExtractModifiedFiles and " +
			"ExtractSummary). Same reason as opencode.go's ReadTranscript.",
		"ExtractPrompts's own sessionRef parameter. Same reason as opencode.go's " +
			"ReadTranscript.",
	},
	"cmd/entire/cli/checkpoint/ephemeral.go": {
		"Dead code today: see the SECURITY comment immediately above this line. This branch " +
			"only runs for a non-incremental TaskStep, and the only production constructor " +
			"(strategy.SaveTaskStep) hard-requires IsIncremental: true.",
		"Dead code today: the SubagentTranscriptPath read in the same non-incremental branch " +
			"as the TranscriptPath read above — see that SECURITY comment.",
	},
	"cmd/entire/cli/checkpoint/persistent.go": {
		"Dead code today: see the SECURITY comment immediately above this line. " +
			"WriteOptions.TranscriptPath is never set by any of its three construction sites " +
			"(manual_commit_condensation.go, attach.go, agentimport/agentimport.go) — all three " +
			"populate Transcript directly with already-confined bytes — and WriteOptions has no " +
			"AgentHome field to confine against even if it were.",
	},
}

// TestUnconfinedTranscriptFileReadsAreLedgered fails the build when a new
// call to the bare agent.ReadTranscriptFile appears without a ledger entry
// explaining why it cannot receive a path sourced from adopted session.State,
// and when a ledgered call disappears without its entry following it.
//
// agent/transcript_file.go itself is excluded: it is where
// agent.ReadTranscriptFile is defined in transcript_file.go, which is excluded:
// its legacy and .entire-cache branches own the dispatch boundary itself.
func TestUnconfinedTranscriptFileReadsAreLedgered(t *testing.T) {
	t.Parallel()

	repoRoot, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}

	// -o, not -c: see transcript_read_guard_test.go's TestTranscriptReadsOnlyShrink
	// for why -c (which counts matching LINES, not matches) would hide a second
	// call added to a line that already had one.
	//
	// Include strategy and review callers as well as agent implementations.
	out := testutil.GitGrepGuard(t, repoRoot, "-o", "-E", "--", unconfinedTranscriptReadPattern,
		"--", ":(glob)cmd/**/*.go",
		":(exclude,glob)**/*_test.go",
		":(exclude)cmd/entire/cli/agent/transcript_file.go")

	found := map[string]int{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		file, _, ok := strings.Cut(line, ":")
		if !ok || !strings.HasSuffix(file, ".go") {
			t.Fatalf("cannot parse git grep output; expected `path:line:match`, got:\n  %s\n"+
				"The filename field is unusable, so this test can prove nothing. "+
				"Check whether git is colorizing into a pipe (color.ui or color.grep set to `always`).", line)
		}
		found[file]++
	}
	if len(found) == 0 {
		t.Fatal("guard matched no calls to agent.ReadTranscriptFile at all; the detection pattern has gone stale and must be re-pointed")
	}

	for file, count := range found {
		reasons, listed := unconfinedTranscriptFileReads[file]
		if !listed {
			t.Errorf("%s calls agent.ReadTranscriptFile and is not in unconfinedTranscriptFileReads.\n"+
				"Either route the read through agent.ReadTranscriptFileUnderHome(path, agentHome) and fail "+
				"closed on a confinement error, or add an entry explaining why this specific call cannot "+
				"receive a path sourced from adopted session.State.", file)
			continue
		}
		if count != len(reasons) {
			t.Errorf("%s has %d calls to agent.ReadTranscriptFile but unconfinedTranscriptFileReads lists %d reasons for it; "+
				"every call needs its own listed reason, one per call.", file, count, len(reasons))
		}
		for i, reason := range reasons {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s: reason #%d in unconfinedTranscriptFileReads is empty", file, i)
			}
		}
	}
	for file := range unconfinedTranscriptFileReads {
		if _, still := found[file]; !still {
			t.Errorf("%s is in unconfinedTranscriptFileReads but no longer calls agent.ReadTranscriptFile; remove its entry.", file)
		}
	}
}
