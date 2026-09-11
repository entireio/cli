package agent_test

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// transcriptReadPattern matches a read of an already-resolved transcript path:
// ReadTranscript(path) and ReadSession(input) receive a path rather than a repo,
// so neither can build a SessionStore, and both call straight into os.
//
// The two variable names are the whole vocabulary these signatures use. Keying
// on them rather than on os.ReadFile in general keeps the guard about the
// transcript-read gap and not about every file the agent packages open.
const transcriptReadPattern = `os\.(ReadFile|Open)\((sessionRef|transcriptPath)\)`

// unconfinedTranscriptReads is the number of unconfined transcript reads each
// file is currently allowed, and why the file has any.
//
// This is a RATCHET, not an allowlist: the set is expected to shrink to nothing
// and must never grow. It exists because the rooting of Entire's I/O is
// knowingly asymmetric here, and the asymmetric half is the one that matters.
// Every WriteSession goes through agent.WriteSessionFile and SessionStore, which
// rejects a SessionRef outside the agent's session directory; the reads below
// have no such boundary, and the read is what pulls transcript content into a
// checkpoint. Closing the gap needs RepoPath on HookInput — a change to the
// external-plugin protocol rather than a refactor — which is exactly the kind of
// work that stalls while the count quietly climbs.
//
// Do NOT close a finding here by anchoring a root on filepath.Dir(sessionRef).
// That puts every component the resolver produced above the root, so it contains
// nothing while looking like it does. See "The Root Anchors" in CLAUDE.md.
var unconfinedTranscriptReads = map[string]int{
	"cmd/entire/cli/agent/claudecode/lifecycle.go":          1,
	"cmd/entire/cli/agent/codex/codex.go":                   1,
	"cmd/entire/cli/agent/codex/transcript.go":              1,
	"cmd/entire/cli/agent/copilotcli/copilotcli.go":         1,
	"cmd/entire/cli/agent/copilotcli/transcript.go":         3,
	"cmd/entire/cli/agent/cursor/lifecycle.go":              1,
	"cmd/entire/cli/agent/cursor/transcript.go":             1,
	"cmd/entire/cli/agent/factoryaidroid/factoryaidroid.go": 1,
	"cmd/entire/cli/agent/factoryaidroid/lifecycle.go":      1,
	"cmd/entire/cli/agent/geminicli/lifecycle.go":           1,
	"cmd/entire/cli/agent/vogon/vogon.go":                   1,

	// The integration harness reads a transcript it wrote itself, in a temp
	// repo it built. Listed rather than excluded by path, so that a real read
	// added under a build tag cannot hide behind the exclusion.
	"cmd/entire/cli/integration_test/hooks.go": 2,
}

// TestTranscriptReadsOnlyShrink fails the build when a new unconfined
// transcript read appears, and when a listed one goes away without the entry
// following it.
//
// Both directions matter. Growth is the regression this exists to stop: a new
// agent integration copies the nearest existing one, so the shape spreads by
// imitation and nobody re-derives whether it is safe. A stale entry is the
// slower failure — a ratchet nobody prunes stops meaning anything, and the
// count is the only record of how much of the gap is left.
func TestTranscriptReadsOnlyShrink(t *testing.T) {
	t.Parallel()

	repoRoot, ok := testutil.GitGrepGuardRepoRoot(t)
	if !ok {
		return
	}

	// -o, not -c: `git grep -c` counts LINES containing a match, so a second
	// read added to a line that already had one is invisible to the ratchet.
	// -o emits one output line per MATCH, which is what the counts below mean.
	//
	// testutil.GitGrepGuard owns --untracked, --no-color and the repo-selector
	// scrubbing, and explains why each is load-bearing for a guard like this.
	out := testutil.GitGrepGuard(t, repoRoot, "-o", "-E", "--", transcriptReadPattern,
		"--", ":(glob)cmd/**/*.go", ":(exclude,glob)**/*_test.go")

	// One line per match, `path:line:matched-text`, so the tally is of matches.
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
		t.Fatal("guard matched no transcript reads at all; the detection pattern has gone stale and must be re-pointed")
	}

	for file, count := range found {
		allowed, listed := unconfinedTranscriptReads[file]
		if !listed {
			t.Errorf("%s reads a transcript by path without a containment boundary, and is not on the ratchet.\n"+
				"Route the read through the agent's SessionStore if you can. If the read genuinely "+
				"cannot be confined yet, add the file to unconfinedTranscriptReads with the reason — "+
				"and do NOT anchor a root on filepath.Dir(sessionRef), which contains nothing.", file)
			continue
		}
		if count > allowed {
			t.Errorf("%s has %d unconfined transcript reads, up from %d. This set may only shrink.", file, count, allowed)
		}
		if count < allowed {
			t.Errorf("%s has %d unconfined transcript reads, down from %d — lower its entry in unconfinedTranscriptReads.", file, count, allowed)
		}
	}
	for file := range unconfinedTranscriptReads {
		if _, still := found[file]; !still {
			t.Errorf("%s is on the ratchet but no longer reads a transcript by path; remove the entry.", file)
		}
	}
}
