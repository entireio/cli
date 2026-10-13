//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// The token invariant harness: every scenario drives a fake agent through the
// real hook entry points, records the tokens it actually spent (the truth),
// and checks after every step that
//
//	(b) at the end, a session's slot token_usage summed over every checkpoint
//	    equals the truth (subagents folded into the parent);
//	(c) after each Stop, the session total (state / `session tokens --json`)
//	    equals the truth so far;
//	(d) neither the checkpoint sum nor the session total ever exceeds the
//	    truth so far;
//	(e) each checkpoint's root token_usage equals the sum of its slots.
//
// (a), the latest cumulative per session, arrives with session_tokens.
//
// A scenario/driver pair that violates an invariant today is listed in
// tokenKnownBugs: it runs, and is skipped with the bug ID if it still
// fails. A known bug that stops reproducing fails the test, so the fix PR
// must delete its entry.
//
// By default Claude Code runs the full scenario table and the other drivers
// run the smoke subset; ENTIRE_TOKEN_MATRIX=full runs everything.

const tokenMatrixEnv = "ENTIRE_TOKEN_MATRIX"

// tokenScenario is one row of the table.
type tokenScenario struct {
	name  string
	smoke bool
	needs []tokenCap
	run   func(h *tokenHarness)
}

// tokenKnownBug is one bug a scenario reproduces on today's CLI, with the
// violations it must produce: "<invariant>:<direction>" keys, where the
// invariant is b, c-st, c-cli, d-cp, d-st or e and the direction is over,
// under, mixed (some fields over, some under) or mismatch (e only).
type tokenKnownBug struct {
	id     string
	reason string
	expect []string
}

// tokenHarness runs one scenario against one driver.
type tokenHarness struct {
	t   *testing.T
	env *TestEnv
	d   tokenAgentDriver
	dir string // worktree new sessions run in
	// agentDir is the driver's agent home (session/project dir) for this env.
	agentDir string
	truth    *tokenTruth

	sessions   []tokenSession
	dirs       map[string]string // session ID -> worktree its hooks run in
	sessionSeq int
	fileSeq    int
	spendSeq   int
	step       string
	// observed maps each violation key ("c-st:under") to its first message.
	observed map[string]string
	// expectState is the sessions whose state file must exist right now.
	expectState map[string]bool
}

func newTokenHarness(t *testing.T, d tokenAgentDriver) *tokenHarness {
	t.Helper()
	env := NewFeatureBranchEnv(t)
	// attach -f amends HEAD through the installed git hooks, which resolve
	// "entire" through PATH; point it at the binary under test.
	env.ExtraEnv = append(env.ExtraEnv,
		"PATH="+filepath.Dir(getTestBinary())+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &tokenHarness{t: t, env: env, d: d, dir: env.RepoDir, truth: newTokenTruth(),
		observed: map[string]string{}, dirs: map[string]string{}, expectState: map[string]bool{}}
	d.Setup(h)
	return h
}

// sessionDir is the worktree a session's hooks run in: where it started, or
// where it was adopted to.
func (h *tokenHarness) sessionDir(id string) string {
	if d, ok := h.dirs[id]; ok {
		return d
	}
	return h.dir
}

func (h *tokenHarness) nextSessionNumber() int {
	h.sessionSeq++
	return h.sessionSeq
}

// next returns a fresh usage whose fields are distinct from every earlier one.
func (h *tokenHarness) next() tokenCounts {
	h.spendSeq++
	k := h.spendSeq
	return h.d.Expressible(tokenCounts{In: 10 + k, Out: 100*k + 1, CacheRead: 1000*k + 7, CacheWrite: 10000*k + 3})
}

func (h *tokenHarness) writeWorktreeFile(dir, path string) {
	h.t.Helper()
	full := filepath.Join(dir, path)
	require.NoError(h.t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(h.t, os.WriteFile(full, []byte(path+" "+h.step+"\n"), 0o644))
}

// violate records a violation under "<invariant>:<direction>", keeping the
// first message per key.
func (h *tokenHarness) violate(invariant, direction, format string, args ...any) {
	key := invariant + ":" + direction
	if _, ok := h.observed[key]; ok {
		return
	}
	h.observed[key] = fmt.Sprintf("after %q: ", h.step) + fmt.Sprintf(format, args...)
}

// direction classifies got against want field by field.
func direction(got, want tokenCounts) string {
	switch {
	case got.atMost(want):
		return "under"
	case want.atMost(got):
		return "over"
	default:
		return "mixed"
	}
}

func scalarDirection(got, want int) string {
	if got < want {
		return "under"
	}
	return "over"
}

// check evaluates (c), (d) and (e). stopped is the session whose Stop (or
// SubagentStop) just ran; its total must equal the truth.
func (h *tokenHarness) check(step string, stopped tokenSession) {
	h.t.Helper()
	h.step = step
	cps, err := readTokenCheckpoints(h.env)
	require.NoError(h.t, err)
	for _, cp := range cps {
		var sum tokenCounts
		for _, s := range cp.Slots {
			sum = sum.plus(s.Usage)
		}
		if sum != cp.Root {
			h.violate("e", "mismatch", "(e) checkpoint %s root token_usage %v != sum of slots %v", cp.ID, cp.Root, sum)
		}
	}
	for _, s := range h.sessions {
		truth := h.truth.of(s.ID())
		if sum := checkpointSumFor(cps, s.ID()); !sum.atMost(truth) {
			h.violate("d-cp", "over", "(d) session %s checkpoints sum %v exceed truth %v:%s",
				s.ID(), sum, truth, describeSlots(cps, s.ID()))
		}
		st, ok, err := stateTokenCounts(h.env, s.ID())
		require.NoError(h.t, err)
		if !ok {
			require.Falsef(h.t, h.expectState[s.ID()], "after %q: session %s has no state file", step, s.ID())
			continue
		}
		if !st.atMost(truth) {
			h.violate("d-st", "over", "(d) session %s status total %v exceeds truth %v", s.ID(), st, truth)
		}
		if stopped == nil || stopped.ID() != s.ID() {
			continue
		}
		if st != truth {
			h.violate("c-st", direction(st, truth), "(c) session %s status total %v != truth %v after Stop", s.ID(), st, truth)
		}
		total, ok, err := sessionTokensTotal(h.env, h.sessionDir(s.ID()), s.ID())
		require.NoError(h.t, err)
		require.Truef(h.t, ok, "after %q: `session tokens` found no session %s", step, s.ID())
		if total != truth.total() {
			h.violate("c-cli", scalarDirection(total, truth.total()),
				"(c) session %s `session tokens --json` total %d != truth %d after Stop", s.ID(), total, truth.total())
		}
	}
}

// finish evaluates (b): every token spent is in exactly one checkpoint.
func (h *tokenHarness) finish() {
	h.t.Helper()
	h.step = "end of scenario"
	cps, err := readTokenCheckpoints(h.env)
	require.NoError(h.t, err)
	for _, s := range h.sessions {
		truth := h.truth.of(s.ID())
		if sum := checkpointSumFor(cps, s.ID()); sum != truth {
			h.violate("b", direction(sum, truth), "(b) session %s checkpoints sum %v != truth %v:%s",
				s.ID(), sum, truth, describeSlots(cps, s.ID()))
		}
	}
}

// --- operations ---

func (h *tokenHarness) newSession() tokenSession {
	s := h.d.NewSession(h)
	h.dirs[s.ID()] = h.dir
	h.sessions = append(h.sessions, s)
	return s
}

// turn starts a turn that writes nFiles new files and makes one API call.
func (h *tokenHarness) turn(s tokenSession, nFiles int) []string {
	h.t.Helper()
	s.StartTurn(fmt.Sprintf("turn %d", h.spendSeq+1))
	h.expectState[s.ID()] = true
	files := make([]string, 0, nFiles)
	for range nFiles {
		files = append(files, h.writeFile(s))
	}
	h.spend(s)
	return files
}

// writeFile makes the running turn write one new file.
func (h *tokenHarness) writeFile(s tokenSession) string {
	h.fileSeq++
	f := fmt.Sprintf("f%d.txt", h.fileSeq)
	s.WriteFile(f)
	return f
}

// spend makes one more API call in the running turn.
func (h *tokenHarness) spend(s tokenSession) {
	h.t.Helper()
	u := h.next()
	s.Spend(u)
	h.truth.add(s.ID(), u)
	h.check("spend", nil)
}

func (h *tokenHarness) subagent(s tokenSession, agentID string) {
	h.t.Helper()
	u := h.next()
	s.SpendSubagent(agentID, u)
	h.truth.add(s.ID(), u)
	// A foreground subagent stops mid-turn; the parent's Stop is where the
	// session total must catch up.
	h.check("subagent "+agentID, nil)
}

func (h *tokenHarness) startBackground(s tokenSession, agentID string) {
	h.t.Helper()
	s.StartBackgroundSubagent(agentID)
	h.check("launch background "+agentID, nil)
}

func (h *tokenHarness) finishBackground(s tokenSession, agentID string) {
	h.t.Helper()
	u := h.next()
	s.FinishSubagent(agentID, u)
	h.truth.add(s.ID(), u)
	h.check("background "+agentID+" stops", s)
}

func (h *tokenHarness) stop(s tokenSession) {
	h.t.Helper()
	s.Stop()
	h.check("stop", s)
}

func (h *tokenHarness) sessionEnd(s tokenSession) {
	h.t.Helper()
	s.SessionEnd()
	h.check("session end", nil)
}

func (h *tokenHarness) commit(files ...string) {
	h.t.Helper()
	h.env.GitCommitWithHooks("user commit "+strings.Join(files, " "), files...)
	h.check("commit "+strings.Join(files, " "), nil)
}

func (h *tokenHarness) agentCommit(files ...string) {
	h.t.Helper()
	h.env.GitCommitWithHooksAsAgent("agent commit "+strings.Join(files, " "), files...)
	h.check("agent commit "+strings.Join(files, " "), nil)
}

// amend runs a real `git commit --amend --no-edit`, so git itself fires
// prepare-commit-msg, commit-msg, post-commit and post-rewrite.
func (h *tokenHarness) amend(files ...string) {
	h.t.Helper()
	h.installGitHooks()
	testutil.RunGit(h.t, h.env.RepoDir, append([]string{"add", "--"}, files...)...)
	cmd := exec.CommandContext(h.t.Context(), "git", "commit", "--amend", "--no-edit")
	cmd.Dir = h.env.RepoDir
	// Amending is a human operation; ENTIRE_TEST_TTY stands in for the terminal.
	cmd.Env = h.env.gitHookEnv("ENTIRE_TEST_TTY=1")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(h.t, err, "git commit --amend: %s", out)
	h.check("amend "+strings.Join(files, " "), nil)
}

// installGitHooks writes Entire's commit hooks into .git/hooks, running the
// binary under test with the arguments the installed hooks pass.
func (h *tokenHarness) installGitHooks() {
	h.t.Helper()
	dir := filepath.Join(h.env.RepoDir, ".git", "hooks")
	require.NoError(h.t, os.MkdirAll(dir, 0o755))
	for name, args := range map[string]string{
		"prepare-commit-msg": `"$1" "$2"`,
		"commit-msg":         `"$1"`,
		"post-commit":        ``,
		"post-rewrite":       `"$1"`,
	} {
		script := fmt.Sprintf("#!/bin/sh\nexec %q hooks git %s %s\n", getTestBinary(), name, args)
		require.NoError(h.t, os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755))
	}
}

// attach commits an unrelated file without hooks and attaches the session.
func (h *tokenHarness) attach(s tokenSession) {
	h.t.Helper()
	h.fileSeq++
	name := fmt.Sprintf("notes-%d.txt", h.fileSeq)
	h.env.WriteFile(name, "notes")
	h.env.GitAdd(name)
	h.env.GitCommit("notes " + name)
	out := h.env.RunCLI("session", "attach", s.ID(), "-a", h.d.AgentName(), "-f")
	require.Contains(h.t, out, "ttach", out)
	h.check("attach", nil)
}

func (h *tokenHarness) snapshot(s tokenSession) {
	h.t.Helper()
	h.env.RunCLI("checkpoint", "create", s.ID())
	h.check("snapshot", nil)
}

func (h *tokenHarness) clearState(s tokenSession) {
	h.t.Helper()
	require.NoError(h.t, h.env.ClearSessionState(s.ID()))
	h.expectState[s.ID()] = false
	h.check("clear session state", nil)
}

// mutateState edits the session's state file in place.
func (h *tokenHarness) mutateState(s tokenSession, fn func(st *session.State)) {
	h.t.Helper()
	st, err := h.env.GetSessionState(s.ID())
	require.NoError(h.t, err)
	require.NotNil(h.t, st, "session %s has no state", s.ID())
	fn(st)
	require.NoError(h.t, h.env.WriteSessionState(s.ID(), st))
}

// killOwner makes the session's recorded agent process look exited.
func (h *tokenHarness) killOwner(s tokenSession) {
	h.mutateState(s, func(st *session.State) {
		st.Owner = &proclive.Identity{PID: os.Getpid(), Start: "not-this-process"}
	})
}

// makeStuck makes an active session look abandoned past the stuck threshold.
func (h *tokenHarness) makeStuck(s tokenSession) {
	h.mutateState(s, func(st *session.State) {
		old := time.Now().Add(-2 * session.StuckActiveThreshold)
		st.LastInteractionTime = &old
		st.Owner = nil
	})
}

// sweep runs the exited-owner sweep through `entire doctor --force`.
func (h *tokenHarness) sweep() {
	h.t.Helper()
	h.t.Logf("doctor --force:\n%s", h.env.RunCLI("doctor", "--force"))
	// The sweep ends the sessions it finalizes; an ended session whose turn
	// left nothing to condense has its state cleaned up.
	for _, s := range h.sessions {
		h.expectState[s.ID()] = false
	}
	h.check("doctor sweep", nil)
}

// skipIfRoot skips scenarios that rely on chmod 000 making a file unreadable.
func (h *tokenHarness) skipIfRoot() {
	if os.Geteuid() == 0 {
		h.t.Skip("chmod 000 does not stop root from reading")
	}
}

func (h *tokenHarness) removePrePromptState(s tokenSession) {
	h.t.Helper()
	err := os.Remove(filepath.Join(h.dir, ".entire", "tmp", "pre-prompt-"+s.ID()+".json"))
	if err != nil && !os.IsNotExist(err) {
		require.NoError(h.t, err)
	}
}

// --- scenarios ---

// tokenScenarios is the table. Each scenario ends with every spent token
// committed, so (b) can demand equality.
func tokenScenarios() []tokenScenario {
	var all []tokenScenario
	for _, group := range [][]tokenScenario{
		turnTokenScenarios(),
		attachTokenScenarios(),
		rewriteTokenScenarios(),
		stopTokenScenarios(),
		sessionMoveTokenScenarios(),
		transcriptTokenScenarios(),
	} {
		all = append(all, group...)
	}
	return all
}

// turnTokenScenarios: turns, commits and mid-turn agent commits.
func turnTokenScenarios() []tokenScenario {
	return []tokenScenario{
		{name: "plain turns", smoke: true, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			b := h.turn(s, 1)
			h.spend(s)
			h.stop(s)
			h.commit(b...)
		}},
		{name: "plain turns with subagent", needs: []tokenCap{capSubagent}, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.subagent(s, "a1111111111111111")
			h.stop(s)
			h.commit(a...)
			b := h.turn(s, 1)
			h.subagent(s, "a2222222222222222")
			h.stop(s)
			h.commit(b...)
		}},
		{name: "partial commit carries forward", run: func(h *tokenHarness) {
			s := h.newSession()
			ab := h.turn(s, 2)
			h.stop(s)
			h.commit(ab[0])
			c := h.turn(s, 1)
			h.stop(s)
			h.commit(append(ab[1:], c...)...)
		}},
		{name: "mid-turn agent commit, usage after", smoke: true, run: func(h *tokenHarness) {
			s := h.newSession()
			ab := h.turn(s, 2)
			h.agentCommit(ab[0])
			h.spend(s)
			h.stop(s)
			h.commit(ab[1])
		}},
		{name: "mid-turn agent commit, no usage after", smoke: true, run: func(h *tokenHarness) {
			s := h.newSession()
			ab := h.turn(s, 2)
			h.agentCommit(ab[0])
			h.stop(s)
			h.commit(ab[1])
		}},
		{name: "mid-turn agent commit, no usage after, attach after stop", needs: []tokenCap{capAttach}, run: func(h *tokenHarness) {
			s := h.newSession()
			ab := h.turn(s, 2)
			h.agentCommit(ab[0])
			h.stop(s)
			h.attach(s)
			h.commit(ab[1])
		}},
		{name: "mid-turn agent commit, no usage after, next turn", run: func(h *tokenHarness) {
			s := h.newSession()
			ab := h.turn(s, 2)
			h.agentCommit(ab[0])
			h.stop(s)
			c := h.turn(s, 1)
			h.stop(s)
			h.commit(append(ab[1:], c...)...)
		}},
	}
}

// attachTokenScenarios: `entire session attach` in each session phase.
func attachTokenScenarios() []tokenScenario {
	return []tokenScenario{
		{name: "attach ended session", smoke: true, needs: []tokenCap{capAttach}, run: func(h *tokenHarness) {
			s := h.newSession()
			ab := h.turn(s, 2)
			h.stop(s)
			h.commit(ab[0])
			h.turn(s, 0)
			h.stop(s)
			h.attach(s)
			c := h.turn(s, 1)
			h.stop(s)
			h.commit(append(ab[1:], c...)...)
		}},
		{name: "attach running session, usage after", smoke: true, needs: []tokenCap{capAttach}, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			b := h.turn(s, 1)
			h.attach(s)
			h.spend(s)
			h.stop(s)
			h.commit(b...)
		}},
		{name: "attach running session, no usage after", needs: []tokenCap{capAttach}, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			b := h.turn(s, 1)
			h.attach(s)
			h.stop(s)
			h.commit(b...)
		}},
		{name: "attach running session, no usage after, next turn", needs: []tokenCap{capAttach}, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			b := h.turn(s, 1)
			h.attach(s)
			h.stop(s)
			c := h.turn(s, 1)
			h.stop(s)
			h.commit(append(b, c...)...)
		}},
		{name: "attach stuck session", needs: []tokenCap{capAttach, capMidTurnTokens}, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			h.turn(s, 0)
			h.makeStuck(s)
			h.attach(s)
		}},
		{name: "attach dead-owner session", needs: []tokenCap{capAttach, capMidTurnTokens}, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			h.turn(s, 0)
			h.killOwner(s)
			h.attach(s)
		}},
	}
}

// rewriteTokenScenarios: amend, cherry-pick and squash.
func rewriteTokenScenarios() []tokenScenario {
	return []tokenScenario{
		{name: "amend after a new turn", run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			b := h.turn(s, 1)
			h.stop(s)
			h.amend(b...)
			c := h.turn(s, 1)
			h.stop(s)
			h.commit(c...)
		}},
		{name: "amend twice", run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			b := h.turn(s, 1)
			h.stop(s)
			h.amend(b...)
			c := h.turn(s, 1)
			h.stop(s)
			h.amend(c...)
		}},
		{name: "amend after a no-file turn", run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			h.turn(s, 0)
			h.stop(s)
			b := h.turn(s, 1)
			h.stop(s)
			h.amend(b...)
		}},
		{name: "cherry-pick then continue", run: func(h *tokenHarness) {
			s := h.newSession()
			testutil.RunGit(h.t, h.env.RepoDir, "branch", "side")
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			picked := h.env.GetHeadHash()
			h.env.GitCheckoutBranch("side")
			testutil.RunGit(h.t, h.env.RepoDir, "cherry-pick", picked)
			h.runGitHook("post-commit")
			h.check("cherry-pick", nil)
			b := h.turn(s, 1)
			h.stop(s)
			h.commit(b...)
		}},
		{name: "squash merge with pending turn", run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			b := h.turn(s, 1)
			h.stop(s)
			h.commit(b...)
			c := h.turn(s, 1)
			h.stop(s)
			h.env.GitCheckoutBranch(masterBranch)
			testutil.RunGit(h.t, h.env.RepoDir, "merge", "--squash", "feature/test-branch")
			h.commit(c...)
		}},
	}
}

// stopTokenScenarios: repeated stops, turns without changes, background subagents and resumes.
func stopTokenScenarios() []tokenScenario {
	return []tokenScenario{
		{name: "repeated stop", smoke: true, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.spend(s)
			h.stop(s)
			h.commit(a...)
		}},
		{name: "repeated stop without pre-prompt state", run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.spend(s)
			h.removePrePromptState(s)
			h.stop(s)
			h.commit(a...)
		}},
		{name: "no-file turn then commit", smoke: true, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			h.turn(s, 0)
			h.stop(s)
			b := h.turn(s, 1)
			h.stop(s)
			h.commit(b...)
		}},
		{name: "no-file turn, session end, resume", needs: []tokenCap{capSessionEnd}, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			h.turn(s, 0)
			h.stop(s)
			h.sessionEnd(s)
			// The tail rides on the session's next checkpoint.
			b := h.turn(s, 1)
			h.stop(s)
			h.commit(b...)
		}},
		{name: "background subagent finishes after stop", needs: []tokenCap{capBackgroundSubagent}, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.startBackground(s, "abg1111111111111")
			h.stop(s)
			h.finishBackground(s, "abg1111111111111")
			h.commit(a...)
		}},
		{name: "background subagent spans turns", needs: []tokenCap{capBackgroundSubagent}, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.startBackground(s, "abg2222222222222")
			h.stop(s)
			h.commit(a...)
			b := h.turn(s, 1)
			h.stop(s)
			h.finishBackground(s, "abg2222222222222")
			h.commit(b...)
		}},
		{name: "resume same machine with tail", smoke: true, run: func(h *tokenHarness) {
			s := h.newSession()
			ab := h.turn(s, 2)
			h.agentCommit(ab...)
			h.spend(s) // tail after the last checkpoint
			h.stop(s)
			c := h.turn(s, 1)
			h.stop(s)
			h.commit(c...)
		}},
		{name: "resume after state cleared with tail", run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			h.turn(s, 0) // tail no checkpoint holds
			h.stop(s)
			h.clearState(s)
			b := h.turn(s, 1)
			h.stop(s)
			h.commit(b...)
		}},
	}
}

// sessionMoveTokenScenarios: sessions copied, imported, moved, crashed, snapshotted or restored.
func sessionMoveTokenScenarios() []tokenScenario {
	return []tokenScenario{
		{name: "fork", needs: []tokenCap{capFork}, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			f := s.Fork()
			h.sessions = append(h.sessions, f)
			b := h.turn(f, 1)
			h.stop(f)
			h.commit(b...)
		}},
		{name: "import overlapping live checkpoints", needs: []tokenCap{capImport}, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			h.env.RunCLI("import", h.d.AgentName())
			h.check("import", nil)
		}},
		{name: "adopt from another worktree", needs: []tokenCap{capWorktreeHooks}, run: func(h *tokenHarness) {
			wt := filepath.Join(h.t.TempDir(), "wt")
			if resolved, err := filepath.EvalSymlinks(filepath.Dir(wt)); err == nil {
				wt = filepath.Join(resolved, "wt")
			}
			testutil.RunGit(h.t, h.env.RepoDir, "worktree", "add", "-b", "other", wt)
			// .entire/ is ignored, so the new worktree needs its own settings.
			settings, err := os.ReadFile(filepath.Join(h.env.RepoDir, ".entire", "settings.json"))
			require.NoError(h.t, err)
			require.NoError(h.t, os.MkdirAll(filepath.Join(wt, ".entire", "tmp"), 0o755))
			require.NoError(h.t, os.WriteFile(filepath.Join(wt, ".entire", "settings.json"), settings, 0o644))
			h.dir = wt
			s := h.newSession()
			h.turn(s, 0) // still running when adopted
			h.dir = h.env.RepoDir
			h.dirs[s.ID()] = h.env.RepoDir
			h.t.Logf("adopt: %s", h.env.RunCLI("session", "adopt", s.ID(), "--from", wt, "--yes"))
			h.check("adopt", nil)
			// The in-flight turn finishes here.
			a := h.writeFile(s)
			h.spend(s)
			h.stop(s)
			h.commit(a)
		}},
		{name: "interrupted turn (no Stop) then next turn", smoke: true, needs: []tokenCap{capMidTurnTokens}, run: func(h *tokenHarness) {
			// Interrupting a turn (Esc in Claude Code) fires no Stop.
			s := h.newSession()
			a := h.turn(s, 1)
			b := h.turn(s, 1)
			h.stop(s)
			h.commit(append(a, b...)...)
		}},
		{name: "crash then exited-owner sweep", needs: []tokenCap{capMidTurnTokens}, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.killOwner(s) // the agent dies before Stop
			h.sweep()
			h.commit(a...)
		}},
		{name: "snapshot then commit", run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			h.turn(s, 0)
			h.stop(s)
			h.snapshot(s)
			b := h.turn(s, 1)
			h.stop(s)
			h.commit(b...)
		}},
		{name: "transcript shrinks on resume restore", needs: []tokenCap{capResumeRestore}, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			h.turn(s, 0) // tail the restore will drop from the local transcript
			h.stop(s)
			h.env.GitCheckoutBranch(masterBranch)
			out, err := h.env.RunResumeForce("feature/test-branch")
			require.NoError(h.t, err, out)
			h.check("session resume --force", nil)
			b := h.turn(s, 1)
			h.stop(s)
			h.commit(b...)
		}},
	}
}

// transcriptTokenScenarios: transcript shapes: compaction, split messages, unreadable or deleted files.
func transcriptTokenScenarios() []tokenScenario {
	return []tokenScenario{
		{name: "compaction", needs: []tokenCap{capCompact}, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
			s.Compact()
			h.check("compact", nil)
			b := h.turn(s, 1)
			h.stop(s)
			h.commit(b...)
		}},
		{name: "split message across commit", needs: []tokenCap{capSplitMessage}, run: func(h *tokenHarness) {
			s := h.newSession()
			ab := h.turn(s, 2)
			partial := h.next()
			s.SpendPartial("msg-split", partial)
			h.truth.add(s.ID(), partial)
			h.check("partial row", nil)
			h.agentCommit(ab[0])
			final := partial
			final.Out += 50
			s.SpendFinal("msg-split", final)
			h.truth.add(s.ID(), tokenCounts{Out: 50})
			h.check("final row", nil)
			h.stop(s)
			h.commit(ab[1])
		}},
		{name: "unreadable transcript at condensation", needs: []tokenCap{capTranscriptTokens}, run: func(h *tokenHarness) {
			s := h.newSession()
			ab := h.turn(s, 2)
			h.stop(s)
			path := s.TranscriptPath()
			h.skipIfRoot()
			require.NoError(h.t, os.Chmod(path, 0o000))
			h.commit(ab[0])
			require.NoError(h.t, os.Chmod(path, 0o600))
			c := h.turn(s, 1)
			h.stop(s)
			h.commit(append(ab[1:], c...)...)
		}},
		{name: "unreadable transcript at mid-turn condensation", needs: []tokenCap{capTranscriptTokens}, run: func(h *tokenHarness) {
			s := h.newSession()
			ab := h.turn(s, 2)
			path := s.TranscriptPath()
			h.skipIfRoot()
			require.NoError(h.t, os.Chmod(path, 0o000))
			h.agentCommit(ab[0])
			require.NoError(h.t, os.Chmod(path, 0o600))
			h.spend(s)
			h.stop(s)
			h.commit(ab[1])
		}},
		{name: "subagent transcript deleted after counted", needs: []tokenCap{capSubagent}, run: func(h *tokenHarness) {
			s := h.newSession()
			a := h.turn(s, 1)
			h.subagent(s, "adel111111111111")
			h.stop(s)
			h.commit(a...)
			s.DeleteSubagentTranscript("adel111111111111")
			b := h.turn(s, 1)
			h.stop(s)
			h.commit(b...)
		}},
	}
}

// runGitHook runs one of Entire's git hooks the way git would after a
// command the harness ran with plain git.
func (h *tokenHarness) runGitHook(name string) {
	h.t.Helper()
	cmd := exec.CommandContext(h.t.Context(), getTestBinary(), "hooks", "git", name)
	cmd.Dir = h.env.RepoDir
	cmd.Env = h.env.gitHookEnv()
	out, err := cmd.CombinedOutput()
	require.NoErrorf(h.t, err, "git hook %s: %s", name, out)
}

// Violation keys known bugs expect.
const (
	bOver     = "b:over"
	bUnder    = "b:under"
	bMixed    = "b:mixed"
	cStOver   = "c-st:over"
	cStUnder  = "c-st:under"
	cStMixed  = "c-st:mixed"
	cCliOver  = "c-cli:over"
	cCliUnder = "c-cli:under"
	dCpOver   = "d-cp:over"
	dStOver   = "d-st:over"
)

// Known bugs. IDs 1-9 and A follow the token-accounting plan; N-prefixed IDs
// were found by this harness. Each lists the violation keys it produces on
// its own; a scenario that combines bugs lists each, and where they overlap
// on a field (an over and an under on the same invariant read as "mixed"),
// the combined key is given to one of them with withKeys.
var (
	bugA = tokenKnownBug{"A", "a mid-turn checkpoint stores the turn so far, then condensation falls back to the " +
		"pending usage, which still holds it", []string{bOver, dCpOver}}
	bug1 = tokenKnownBug{"1", "a repeated Stop counts the turn again from its start", []string{cStOver, cCliOver, dStOver}}
	bug2 = tokenKnownBug{"2", "new state for a session with an existing transcript counts the transcript from line 0",
		[]string{bOver, dCpOver}}
	bug3 = tokenKnownBug{"3", "import writes checkpoints for turns live checkpoints already hold", []string{bOver, dCpOver}}
	bug4 = tokenKnownBug{"4", "amending keeps the amended checkpoint's tokens and defers the new turn's to the next " +
		"commit; they are lost only if no commit follows", []string{bUnder}}
	bug5 = tokenKnownBug{"5", "a turn without file changes is missing from the session total until a checkpoint " +
		"recounts it", []string{cStUnder, cCliUnder}}
	bug6 = tokenKnownBug{"6", "a message streamed across a checkpoint is counted in both", []string{bOver, dCpOver}}
	bug8 = tokenKnownBug{"8", "a background subagent that stops after its parent's Stop is missing from the session total",
		[]string{cStUnder, cCliUnder}}
	bug9  = tokenKnownBug{"9", "the checkpoint after a snapshot recounts what the snapshot stored", []string{bOver, dCpOver}}
	bugN1 = tokenKnownBug{"N1", "a turn whose agent dies before Stop is finalized without a step: its tokens reach " +
		"no checkpoint", []string{bUnder}}
	bugN2 = tokenKnownBug{"N2", "session resume --force restores a shorter transcript and the dropped tail reaches " +
		"no checkpoint", []string{bUnder}}
	bugN4 = tokenKnownBug{"N4", "a turn that never gets Stop (interrupted) is missing from the session total once " +
		"the next prompt starts", []string{cStUnder, cCliUnder}}
	bugN5 = tokenKnownBug{"N5", "Copilot: condensation backfills the session total from line 0, then Stop adds the " +
		"whole turn again", []string{cStMixed, dStOver}}
	bugN6 = tokenKnownBug{"N6", "Copilot: input and cache tokens reach events.jsonl only in session.shutdown, so " +
		"until the session ends neither the session total nor checkpoints carry them", []string{bUnder, cStUnder, cCliUnder}}
	bugN7 = tokenKnownBug{"N7", "if agy's total_input_tokens includes cache reads (unverified), cache reads are " +
		"counted twice", []string{bOver, cStOver, cCliOver, dCpOver, dStOver}}
)

// withKeys returns the bug expecting keys instead of its usual ones.
func (kb tokenKnownBug) withKeys(keys ...string) tokenKnownBug {
	kb.expect = keys
	return kb
}

func bugs(kbs ...tokenKnownBug) []tokenKnownBug { return kbs }

// tokenKnownBugs maps "scenario/driver" (driver "*" for all) to the bugs it
// reproduces. An exact driver entry replaces the wildcard; an empty one means
// the driver reproduces none.
var tokenKnownBugs = map[string][]tokenKnownBug{
	"mid-turn agent commit, no usage after/*":                    bugs(bugA),
	"mid-turn agent commit, no usage after, attach after stop/*": bugs(bugA),
	"attach running session, no usage after/*":                   bugs(bugA),
	"attach ended session/*":                                     bugs(bug5),
	"amend twice/*":                                              bugs(bug4),
	"amend after a no-file turn/*":                               bugs(bug4, bug5),
	"repeated stop/*":                                            bugs(bug1),
	"repeated stop without pre-prompt state/*":                   bugs(bug1),
	"no-file turn then commit/*":                                 bugs(bug5),
	"no-file turn, session end, resume/*":                        bugs(bug5),
	"background subagent finishes after stop/*":                  bugs(bug8),
	"background subagent spans turns/*":                          bugs(bug8),
	"resume same machine with tail/*":                            bugs(bug5),
	"resume after state cleared with tail/*":                     bugs(bug5, bug2),
	"fork/*":                                                     bugs(bug2),
	"import overlapping live checkpoints/*":                      bugs(bug3),
	"interrupted turn (no Stop) then next turn/*":                bugs(bugN4),
	"crash then exited-owner sweep/*":                            bugs(bugN1),
	"snapshot then commit/*":                                     bugs(bug5, bug9),
	"transcript shrinks on resume restore/*":                     bugs(bug5, bugN2),
	"split message across commit/*":                              bugs(bug6),

	// Cursor reports a turn's tokens only in its Stop payload: nothing is
	// consumed mid-turn (no A), and a turn without changes or a repeated Stop
	// reaches checkpoints the way it reaches the session total.
	"mid-turn agent commit, no usage after/cursor":                    nil,
	"mid-turn agent commit, no usage after, attach after stop/cursor": nil,
	"attach running session, no usage after/cursor":                   nil,
	"attach ended session/cursor":                                     bugs(bug5.withKeys(bUnder, cStUnder, cCliUnder)),
	"no-file turn then commit/cursor":                                 bugs(bug5.withKeys(bUnder, cStUnder, cCliUnder)),
	"no-file turn, session end, resume/cursor":                        bugs(bug5.withKeys(bUnder, cStUnder, cCliUnder)),
	"resume same machine with tail/cursor":                            bugs(bug5.withKeys(bUnder, cStUnder, cCliUnder)),
	"resume after state cleared with tail/cursor":                     bugs(bug5.withKeys(bUnder, cStUnder, cCliUnder)),
	"snapshot then commit/cursor":                                     bugs(bug5.withKeys(bUnder, cStUnder, cCliUnder)),
	"amend after a no-file turn/cursor":                               bugs(bug4, bug5),
	"repeated stop/cursor": bugs(bug1.withKeys(bOver, cStOver, cCliOver, dCpOver, dStOver).
		withReason("a repeated Stop re-sending the same generation_id is counted again, in the session total and the checkpoint")),
	"repeated stop without pre-prompt state/cursor": bugs(bug1.withKeys(bOver, cStOver, cCliOver, dCpOver, dStOver).
		withReason("a repeated Stop re-sending the same generation_id is counted again, in the session total and the checkpoint")),

	// Antigravity reads cumulative statusline snapshots out of band: no-change
	// turns are recorded at Stop (no 5) and nothing is consumed mid-turn (no A).
	"mid-turn agent commit, no usage after/antigravity": nil,
	"no-file turn then commit/antigravity":              nil,
	"resume same machine with tail/antigravity":         nil,
	"amend after a no-file turn/antigravity":            bugs(bug4),
	"snapshot then commit/antigravity":                  bugs(bug9),
	"repeated stop/antigravity": bugs(bug1.withKeys(bUnder, cStUnder, cCliUnder).
		withReason("the second Stop drops the tokens spent since the first")),
	"repeated stop without pre-prompt state/antigravity": bugs(bug1.withKeys(bUnder, cStUnder, cCliUnder).
		withReason("the second Stop drops the tokens spent since the first")),
	"resume after state cleared with tail/antigravity": bugs(bug2.withKeys(bUnder, cStUnder, cCliUnder).
		withReason("new state drops the snapshot baseline and the tail with it")),
	"plain turns/antigravity-cache-in-total": bugs(bugN7),

	"subagent transcript deleted after counted/codex": bugs(bug8.withReason(
		"the session total re-reads child rollouts, so a deleted child's counted tokens drop out")),

	// Copilot: N6 hides input and cache tokens everywhere until the session
	// ends; where it overlaps an over-count the combined field reads "mixed".
	"plain turns/copilot-cli":                                       bugs(bugN6),
	"partial commit carries forward/copilot-cli":                    bugs(bugN6),
	"attach running session, usage after/copilot-cli":               bugs(bugN6),
	"attach running session, no usage after, next turn/copilot-cli": bugs(bugN6),
	"attach stuck session/copilot-cli":                              bugs(bugN6),
	"attach dead-owner session/copilot-cli":                         bugs(bugN6),
	"amend after a new turn/copilot-cli":                            bugs(bugN6),
	"cherry-pick then continue/copilot-cli":                         bugs(bugN6),
	"squash merge with pending turn/copilot-cli":                    bugs(bugN6),
	"unreadable transcript at condensation/copilot-cli":             bugs(bugN6),
	"unreadable transcript at mid-turn condensation/copilot-cli":    bugs(bugN6),
	"attach ended session/copilot-cli":                              bugs(bug5, bugN6),
	"no-file turn then commit/copilot-cli":                          bugs(bug5, bugN6),
	"no-file turn, session end, resume/copilot-cli":                 bugs(bug5, bugN6),
	"resume same machine with tail/copilot-cli":                     bugs(bug5, bugN6),
	"interrupted turn (no Stop) then next turn/copilot-cli":         bugs(bugN4, bugN6),
	"amend twice/copilot-cli":                                       bugs(bug4, bugN6),
	"amend after a no-file turn/copilot-cli":                        bugs(bug4, bug5, bugN6),
	"mid-turn agent commit, usage after/copilot-cli":                bugs(bugN5, bugN6.withKeys(bUnder, cCliUnder)),
	"mid-turn agent commit, no usage after, next turn/copilot-cli":  bugs(bugN5, bugN6.withKeys(bUnder, cCliUnder)),
	"mid-turn agent commit, no usage after/copilot-cli": bugs(bugA.withKeys(bMixed, dCpOver), bugN5,
		bugN6.withKeys(cCliUnder)),
	"mid-turn agent commit, no usage after, attach after stop/copilot-cli": bugs(bugA.withKeys(bMixed, dCpOver), bugN5,
		bugN6.withKeys(cCliUnder)),
	"attach running session, no usage after/copilot-cli": bugs(bugA.withKeys(bMixed, dCpOver), bugN6.withKeys(cStUnder, cCliUnder)),
	"repeated stop/copilot-cli":                          bugs(bug1.withKeys(cStMixed, dStOver), bugN6),
	"repeated stop without pre-prompt state/copilot-cli": bugs(bug1.withKeys(cStMixed, dStOver), bugN6),
	"resume after state cleared with tail/copilot-cli":   bugs(bug5, bug2.withKeys(bMixed, dCpOver), bugN6.withKeys(cStUnder, cCliUnder)),
	"snapshot then commit/copilot-cli":                   bugs(bug5, bug9.withKeys(bMixed, dCpOver), bugN6.withKeys(cStUnder, cCliUnder)),
}

// withReason returns the bug with a driver-specific explanation.
func (kb tokenKnownBug) withReason(reason string) tokenKnownBug {
	kb.reason = reason
	return kb
}

func lookupKnownBugs(scenario, driver string) []tokenKnownBug {
	if kbs, ok := tokenKnownBugs[scenario+"/"+driver]; ok {
		return kbs
	}
	return tokenKnownBugs[scenario+"/*"]
}

func tokenDrivers() []tokenAgentDriver {
	return []tokenAgentDriver{
		claudeTokenDriver{},
		newCursorTokenDriver(),
		newCodexTokenDriver(),
		newCopilotTokenDriver(),
		newDroidTokenDriver(),
		newOpenCodeTokenDriver(),
		newPiTokenDriver(),
		newAntigravityTokenDriver(),
		newAntigravityCacheInTotalDriver(),
		newExternalTokenDriver(),
		newVogonTokenDriver(),
	}
}

// tokenMatrix collects one cell per scenario x driver for the summary log.
type tokenMatrix struct {
	mu    sync.Mutex
	cells map[string]map[string]string
}

func (m *tokenMatrix) set(scenario, driver, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cells[scenario] == nil {
		m.cells[scenario] = map[string]string{}
	}
	m.cells[scenario][driver] = result
}

func (m *tokenMatrix) render(drivers []tokenAgentDriver, scenarios []tokenScenario) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	b.WriteString("| scenario |")
	for _, d := range drivers {
		b.WriteString(" " + d.Name() + " |")
	}
	b.WriteString("\n|---|" + strings.Repeat("---|", len(drivers)) + "\n")
	for _, sc := range scenarios {
		b.WriteString("| " + sc.name + " |")
		for _, d := range drivers {
			cell := m.cells[sc.name][d.Name()]
			if cell == "" {
				cell = "-"
			}
			b.WriteString(" " + cell + " |")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func TestTokenInvariants(t *testing.T) {
	t.Parallel()
	full := os.Getenv(tokenMatrixEnv) == "full"
	drivers := tokenDrivers()
	scenarios := tokenScenarios()
	matrix := &tokenMatrix{cells: map[string]map[string]string{}}
	start := time.Now()
	t.Cleanup(func() {
		t.Logf("token invariant matrix (%s):\n%s", time.Since(start).Round(time.Second), matrix.render(drivers, scenarios))
	})
	for _, d := range drivers {
		t.Run(d.Name(), func(t *testing.T) {
			t.Parallel()
			caps := d.Caps()
			for _, sc := range scenarios {
				t.Run(sc.name, func(t *testing.T) {
					t.Parallel()
					for _, c := range sc.needs {
						if !caps[c] {
							matrix.set(sc.name, d.Name(), "n/s")
							t.Skipf("not supported: driver %s lacks %s", d.Name(), c)
						}
					}
					if !d.Runs(sc.name) {
						matrix.set(sc.name, d.Name(), "n/s")
						t.Skipf("driver %s runs only its own scenarios", d.Name())
					}
					if !sc.smoke && !d.FullMatrix() && !full {
						matrix.set(sc.name, d.Name(), "smoke-only")
						t.Skipf("smoke subset only; set %s=full", tokenMatrixEnv)
					}
					runTokenScenario(t, d, sc, matrix)
				})
			}
		})
	}
}

func runTokenScenario(t *testing.T, d tokenAgentDriver, sc tokenScenario, matrix *tokenMatrix) {
	t.Helper()
	h := newTokenHarness(t, d)
	sc.run(h)
	h.finish()
	kbs := lookupKnownBugs(sc.name, d.Name())
	result, problems := judgeKnownBugs(h.observed, kbs)
	matrix.set(sc.name, d.Name(), result)
	var observed []string
	for key, msg := range h.observed {
		observed = append(observed, key+"  "+msg)
	}
	sort.Strings(observed)
	if len(problems) > 0 {
		t.Errorf("%s\nobserved violations:\n%s", strings.Join(problems, "\n"), strings.Join(observed, "\n"))
		return
	}
	if len(kbs) > 0 {
		t.Skipf("known bugs %s:\n%s", result, strings.Join(observed, "\n"))
	}
}

// judgeKnownBugs compares observed violation keys with the known bugs'
// expected keys. Any unexpected key fails, and so does an expected key that
// no longer appears: a fix must delete (or narrow) its entry.
func judgeKnownBugs(observed map[string]string, kbs []tokenKnownBug) (string, []string) {
	expected := map[string]bool{}
	var problems, ids []string
	for _, kb := range kbs {
		ids = append(ids, kb.id)
		for _, key := range kb.expect {
			expected[key] = true
			if _, ok := observed[key]; !ok {
				problems = append(problems, fmt.Sprintf("knownBug %s (%s): expected violation %s no longer appears; update tokenKnownBugs", kb.id, kb.reason, key))
			}
		}
	}
	var unexpected []string
	for key := range observed {
		if !expected[key] {
			unexpected = append(unexpected, key)
		}
	}
	sort.Strings(unexpected)
	for _, key := range unexpected {
		problems = append(problems, "unexpected violation "+key+": "+observed[key])
	}
	switch {
	case len(unexpected) > 0:
		return "FAIL[" + strings.Join(unexpected, ",") + "]", problems
	case len(problems) > 0:
		return "STALE[" + strings.Join(ids, "+") + "]", problems
	case len(ids) > 0:
		return strings.Join(ids, "+"), nil
	default:
		return "pass", nil
	}
}
