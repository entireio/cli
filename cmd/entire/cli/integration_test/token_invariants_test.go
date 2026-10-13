//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// tokenKnownBug marks a scenario that violates an invariant on today's CLI.
type tokenKnownBug struct {
	id     string
	reason string
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
	violations []string
	seen       map[string]bool
}

func newTokenHarness(t *testing.T, d tokenAgentDriver) *tokenHarness {
	t.Helper()
	env := NewFeatureBranchEnv(t)
	// attach -f amends HEAD through the installed git hooks, which resolve
	// "entire" through PATH; point it at the binary under test.
	env.ExtraEnv = append(env.ExtraEnv,
		"PATH="+filepath.Dir(getTestBinary())+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := &tokenHarness{t: t, env: env, d: d, dir: env.RepoDir, truth: newTokenTruth(),
		seen: map[string]bool{}, dirs: map[string]string{}}
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

func (h *tokenHarness) violate(key, format string, args ...any) {
	if h.seen[key] {
		return
	}
	h.seen[key] = true
	h.violations = append(h.violations, fmt.Sprintf("after %q: ", h.step)+fmt.Sprintf(format, args...))
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
			h.violate("e:"+cp.ID, "(e) checkpoint %s root token_usage %v != sum of slots %v", cp.ID, cp.Root, sum)
		}
	}
	for _, s := range h.sessions {
		truth := h.truth.of(s.ID())
		if sum := checkpointSumFor(cps, s.ID()); !sum.atMost(truth) {
			h.violate("d-cp:"+s.ID(), "(d) session %s checkpoints sum %v exceed truth %v:%s",
				s.ID(), sum, truth, describeSlots(cps, s.ID()))
		}
		st, ok, err := stateTokenCounts(h.env, s.ID())
		require.NoError(h.t, err)
		if !ok {
			continue
		}
		if !st.atMost(truth) {
			h.violate("d-st:"+s.ID(), "(d) session %s status total %v exceeds truth %v", s.ID(), st, truth)
		}
		if stopped == nil || stopped.ID() != s.ID() {
			continue
		}
		if st != truth {
			h.violate("c-st:"+s.ID(), "(c) session %s status total %v != truth %v after Stop", s.ID(), st, truth)
		}
		total, ok, err := sessionTokensTotal(h.env, h.dir, s.ID())
		require.NoError(h.t, err)
		if ok && total != truth.total() {
			h.violate("c-cli:"+s.ID(), "(c) session %s `session tokens --json` total %d != truth %d after Stop",
				s.ID(), total, truth.total())
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
			h.violate("b:"+s.ID(), "(b) session %s checkpoints sum %v != truth %v:%s",
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
	files := make([]string, 0, nFiles)
	for range nFiles {
		h.fileSeq++
		f := fmt.Sprintf("f%d.txt", h.fileSeq)
		s.WriteFile(f)
		files = append(files, f)
	}
	h.spend(s)
	return files
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

func (h *tokenHarness) amend(files ...string) {
	h.t.Helper()
	h.env.GitCommitAmendWithHooks("amended "+strings.Join(files, " "), files...)
	h.check("amend "+strings.Join(files, " "), nil)
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
	h.check("doctor sweep", nil)
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
			a := h.turn(s, 1)
			h.stop(s)
			h.commit(a...)
		}},
		{name: "crash then exited-owner sweep", run: func(h *tokenHarness) {
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

// tokenKnownBugs maps "scenario/driver" (driver "*" for all) to the bug it
// reproduces. Bug IDs follow the token-accounting plan.
//
// IDs: A mid-turn consumption recounted; 1 repeated Stop recounts from line 0;
// 2 new state on an existing transcript; 3 import overlaps live checkpoints;
// 4 amend overwrites the slot; 5 no-change turns missing from status;
// 6 split message counted twice; 8 subagent totals; 9 snapshot overlap.
// N-prefixed IDs were found by this harness and are not in the plan's list.
var tokenKnownBugs = map[string]tokenKnownBug{
	"mid-turn agent commit, no usage after/*": {"A",
		"condensation falls back to the pending usage, which holds the turn the mid-turn checkpoint already stored"},
	"mid-turn agent commit, no usage after, attach after stop/*": {"A",
		"attach falls back to the pending usage the mid-turn checkpoint already stored"},
	"attach running session, no usage after/*": {"A",
		"the turn's checkpoint falls back to pending usage that attach already stored"},
	"attach ended session/*":                   {"5", "a no-change turn's Stop leaves the session total behind"},
	"repeated stop/*":                          {"1", "the second Stop counts the turn again from line 0"},
	"repeated stop without pre-prompt state/*": {"1", "the second Stop counts the turn again from line 0"},
	"no-file turn then commit/*":               {"5", "a no-change turn's Stop leaves the session total behind"},
	"no-file turn, session end, resume/*":      {"5", "a no-change turn's Stop leaves the session total behind"},
	"resume same machine with tail/*": {"5",
		"a turn whose files were all committed mid-turn ends with no changes, so its Stop never counts it"},
	"resume after state cleared with tail/*": {"5+2",
		"no-change turn missing from status; the new state recounts the transcript from line 0"},
	"fork/*":                                {"2", "the forked session's new state counts the copied history"},
	"import overlapping live checkpoints/*": {"3", "import writes checkpoints for turns live checkpoints already hold"},
	"amend twice/*":                         {"4", "amending a commit keeps the old checkpoint and drops the new turn's tokens"},
	"amend after a no-file turn/*":          {"4+5", "amending drops the new turns' tokens; no-change turn missing from status"},
	"snapshot then commit/*": {"5+9",
		"no-change turn missing from status; the next checkpoint recounts what the snapshot stored"},
	"split message across commit/*":             {"6", "a message split across a checkpoint is counted in both"},
	"background subagent finishes after stop/*": {"8", "a background subagent's SubagentStop after Stop leaves the session total behind"},
	"background subagent spans turns/*":         {"8", "a background subagent's SubagentStop after Stop leaves the session total behind"},
	"crash then exited-owner sweep/*": {"N1",
		"a turn whose agent dies before Stop is finalized without a step: its files and tokens reach no checkpoint"},
	"transcript shrinks on resume restore/*": {"5+N2",
		"session resume --force restores a shorter transcript and the dropped tail reaches no checkpoint"},
	"adopt from another worktree/*": {"N3", "adopting a running session drops the turn's pre-adopt tokens from the session total"},

	// Per-driver overrides. An empty ID means the driver does not reproduce
	// the wildcard bug: Cursor and Antigravity report a turn's tokens at its
	// Stop, so nothing is consumed mid-turn, and Antigravity's out-of-band
	// source records turns without file changes.
	"mid-turn agent commit, no usage after/cursor":                    {},
	"mid-turn agent commit, no usage after, attach after stop/cursor": {},
	"attach running session, no usage after/cursor":                   {},
	"mid-turn agent commit, no usage after/antigravity":               {},
	"no-file turn then commit/antigravity":                            {},
	"resume same machine with tail/antigravity":                       {},
	"subagent transcript deleted after counted/codex": {"8",
		"the session total re-reads child rollouts, so a deleted child's counted tokens drop out"},
	"mid-turn agent commit, usage after/copilot-cli": {"8",
		"condensation backfills the session total from line 0, then Stop adds the whole turn again"},
	"mid-turn agent commit, no usage after, next turn/copilot-cli": {"8",
		"condensation backfills the session total from line 0, then Stop adds the whole turn again"},
}

func lookupKnownBug(scenario, driver string) (tokenKnownBug, bool) {
	if kb, ok := tokenKnownBugs[scenario+"/"+driver]; ok {
		return kb, kb.id != ""
	}
	kb, ok := tokenKnownBugs[scenario+"/*"]
	return kb, ok
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
	kb, known := lookupKnownBug(sc.name, d.Name())
	switch {
	case len(h.violations) > 0 && known:
		matrix.set(sc.name, d.Name(), "bug "+kb.id)
		t.Skipf("knownBug %s: %s\n%s", kb.id, kb.reason, strings.Join(h.violations, "\n"))
	case len(h.violations) > 0:
		matrix.set(sc.name, d.Name(), "FAIL")
		t.Errorf("token invariants violated:\n%s", strings.Join(h.violations, "\n"))
	case known:
		matrix.set(sc.name, d.Name(), "FIXED?")
		t.Errorf("knownBug %s (%s) no longer reproduces; remove its tokenKnownBugs entry", kb.id, kb.reason)
	default:
		matrix.set(sc.name, d.Name(), "pass")
	}
}
