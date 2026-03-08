//go:build hookperf

package strategy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

const hookPerfRepoURL = "https://github.com/entireio/cli.git"

// TestCommitHookPerformance measures the real overhead of Entire's commit hooks
// by comparing a control commit (no Entire) against a commit with hooks active.
//
// It uses a full-history clone of entireio/cli (single branch) with seeded
// branches and packed refs so that go-git operates on a realistic object
// database. Each session is generated with a unique base commit (drawn from
// real repo history) so that listAllSessionStates scans different shadow
// branch names — matching production behavior where sessions span many commits.
//
// Prerequisites:
//   - GitHub access (gh auth login) for cloning the private repo
//
// Run: go test -v -run TestCommitHookPerformance -tags hookperf -timeout 15m ./cmd/entire/cli/strategy/
func TestCommitHookPerformance(t *testing.T) {
	// Clone once, reuse across scenarios via cheap local clones.
	cacheDir := cloneSourceRepo(t)

	scenarios := []struct {
		name   string
		ended  int
		idle   int
		active int
	}{
		{"100sessions", 88, 11, 1},
		{"200sessions", 176, 22, 2},
		{"500sessions", 440, 55, 5},
	}

	type result struct {
		name       string
		total      int
		control    time.Duration
		prepare    time.Duration
		postCommit time.Duration
	}
	results := make([]result, 0, len(scenarios))

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			totalSessions := sc.ended + sc.idle + sc.active

			dir := localClone(t, cacheDir)
			t.Chdir(dir)
			paths.ClearWorktreeRootCache()
			session.ClearGitCommonDirCache()

			// Seed 200 branches + pack refs for realistic ref scanning overhead.
			seedBranches(t, dir, 200)
			gitRun(t, dir, "pack-refs", "--all")

			// --- CONTROL: commit without Entire ---
			controlDur := timeControlCommit(t, dir)

			// Reset back to pre-commit state so the test commit is identical.
			gitRun(t, dir, "reset", "HEAD~1")
			gitRun(t, dir, "add", "perf_control.txt")

			// --- TEST: commit with Entire hooks ---
			createHookPerfSettings(t, dir)

			// Collect diverse base commits from real repo history so each
			// ENDED session has a different shadow branch name.
			baseCommits := collectBaseCommits(t, dir, totalSessions)
			seedHookPerfSessions(t, dir, baseCommits, sc.ended, sc.idle, sc.active)

			// Simulate TTY path with commit_linking=always.
			t.Setenv("ENTIRE_TEST_TTY", "1")
			paths.ClearWorktreeRootCache()
			session.ClearGitCommonDirCache()

			commitMsgFile := filepath.Join(dir, ".git", "COMMIT_EDITMSG")
			if err := os.WriteFile(commitMsgFile, []byte("implement feature\n"), 0o644); err != nil {
				t.Fatalf("write commit msg: %v", err)
			}

			s1 := &ManualCommitStrategy{}
			prepStart := time.Now()
			if err := s1.PrepareCommitMsg(context.Background(), commitMsgFile, "message"); err != nil {
				t.Fatalf("PrepareCommitMsg: %v", err)
			}
			prepDur := time.Since(prepStart)

			// Read back commit message; inject trailer if content-aware check skipped it.
			msgBytes, err := os.ReadFile(commitMsgFile) //nolint:gosec // test file
			if err != nil {
				t.Fatalf("read commit msg: %v", err)
			}
			commitMsg := string(msgBytes)

			if _, found := trailers.ParseCheckpoint(commitMsg); !found {
				cpID, genErr := id.Generate()
				if genErr != nil {
					t.Fatalf("generate checkpoint ID: %v", genErr)
				}
				commitMsg = fmt.Sprintf("%s\n%s: %s\n",
					strings.TrimRight(commitMsg, "\n"),
					trailers.CheckpointTrailerKey, cpID)
				t.Logf("  Injected trailer (PrepareCommitMsg skipped content-aware check)")
			}

			gitRun(t, dir, "commit", "-m", commitMsg)

			// Time PostCommit.
			paths.ClearWorktreeRootCache()
			session.ClearGitCommonDirCache()

			s2 := &ManualCommitStrategy{}
			postStart := time.Now()
			if err := s2.PostCommit(context.Background()); err != nil {
				t.Fatalf("PostCommit: %v", err)
			}
			postDur := time.Since(postStart)

			overhead := (prepDur + postDur) - controlDur
			if overhead < 0 {
				overhead = 0
			}

			t.Logf("=== %s ===", sc.name)
			t.Logf("  Sessions:         %d (ended=%d, idle=%d, active=%d)", totalSessions, sc.ended, sc.idle, sc.active)
			t.Logf("  Base commits:     %d unique", len(baseCommits))
			t.Logf("  Control commit:   %s", controlDur.Round(time.Millisecond))
			t.Logf("  PrepareCommitMsg: %s", prepDur.Round(time.Millisecond))
			t.Logf("  PostCommit:       %s", postDur.Round(time.Millisecond))
			t.Logf("  TOTAL HOOKS:      %s", (prepDur + postDur).Round(time.Millisecond))
			t.Logf("  OVERHEAD:         %s", overhead.Round(time.Millisecond))

			results = append(results, result{
				name:       sc.name,
				total:      totalSessions,
				control:    controlDur,
				prepare:    prepDur,
				postCommit: postDur,
			})
		})
	}

	// Print comparison table.
	t.Log("")
	t.Log("========== COMMIT HOOK PERFORMANCE ==========")
	t.Logf("%-14s | %8s | %10s | %10s | %12s | %12s | %10s",
		"Scenario", "Sessions", "Control", "Prepare", "PostCommit", "Total+Hooks", "Overhead")
	t.Log(strings.Repeat("-", 95))
	for _, r := range results {
		total := r.prepare + r.postCommit
		overhead := total - r.control
		if overhead < 0 {
			overhead = 0
		}
		t.Logf("%-14s | %8d | %10s | %10s | %12s | %12s | %10s",
			r.name,
			r.total,
			r.control.Round(time.Millisecond),
			r.prepare.Round(time.Millisecond),
			r.postCommit.Round(time.Millisecond),
			total.Round(time.Millisecond),
			overhead.Round(time.Millisecond),
		)
	}
}

// collectBaseCommits walks the repo's commit history and returns up to `need`
// unique commit hashes. These are used as BaseCommit values so each session
// references a different shadow branch name — matching production behavior
// where sessions span many different commits over time.
func collectBaseCommits(t *testing.T, dir string, need int) []string {
	t.Helper()

	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open repo for base commits: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head for base commits: %v", err)
	}

	var commits []string
	iter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		t.Fatalf("log for base commits: %v", err)
	}
	defer iter.Close()

	err = iter.ForEach(func(c *object.Commit) error {
		if len(commits) >= need {
			return fmt.Errorf("done") //nolint:goerr113 // sentinel to stop iteration
		}
		commits = append(commits, c.Hash.String())
		return nil
	})
	// "done" sentinel is expected; real errors are not.
	if err != nil && err.Error() != "done" {
		t.Fatalf("walk commits: %v", err)
	}

	t.Logf("  Collected %d base commits from history (requested %d)", len(commits), need)
	return commits
}

// timeControlCommit stages a file and times a bare `git commit` with no Entire
// hooks/settings present. Returns the wall-clock duration.
func timeControlCommit(t *testing.T, dir string) time.Duration {
	t.Helper()

	controlFile := filepath.Join(dir, "perf_control.txt")
	if err := os.WriteFile(controlFile, []byte("control commit content\n"), 0o644); err != nil {
		t.Fatalf("write control file: %v", err)
	}
	gitRun(t, dir, "add", "perf_control.txt")

	start := time.Now()
	gitRun(t, dir, "commit", "-m", "control commit (no Entire)")
	return time.Since(start)
}

// seedBranches creates N branches pointing at HEAD via go-git to simulate
// a repo with many refs (affects ref scanning performance).
func seedBranches(t *testing.T, dir string, count int) {
	t.Helper()

	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open repo for branch seeding: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head for branch seeding: %v", err)
	}
	headHash := head.Hash()

	for i := range count {
		name := fmt.Sprintf("feature/perf-branch-%03d", i)
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), headHash)
		if err := repo.Storer.SetReference(ref); err != nil {
			t.Fatalf("create branch %s: %v", name, err)
		}
	}
	t.Logf("  Seeded %d branches", count)
}

// cloneSourceRepo does a one-time full-history clone of entireio/cli into a temp
// directory. Returns the path to use as a local clone source for each scenario.
//
// Uses --single-branch to limit network transfer to one branch while still
// fetching the full commit history and object database. This gives us a
// realistic packfile (~50-100MB) instead of a shallow clone's ~900KB, which
// matters because go-git object resolution (tree.File, commit.Tree, file.Contents)
// performance depends on packfile size and index complexity.
func cloneSourceRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	t.Logf("Cloning %s (full history, single branch) ...", hookPerfRepoURL)
	start := time.Now()

	//nolint:gosec // test-only, URL is a constant
	cmd := exec.Command("git", "clone", "--single-branch", hookPerfRepoURL, dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git clone failed: %v\n%s", err, out)
	}
	t.Logf("Source clone completed in %s", time.Since(start).Round(time.Millisecond))

	return dir
}

// localClone creates a fast local clone from the cached source repo.
func localClone(t *testing.T, sourceDir string) string {
	t.Helper()

	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	//nolint:gosec // test-only, sourceDir is from t.TempDir()
	cmd := exec.Command("git", "clone", "--local", sourceDir, dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("local clone failed: %v\n%s", err, out)
	}

	return dir
}

// gitRun executes a git command in the given directory and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	//nolint:gosec // test-only helper
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// createHookPerfSettings writes .entire/settings.json with commit_linking=always
// so PrepareCommitMsg auto-links without prompting.
func createHookPerfSettings(t *testing.T, dir string) {
	t.Helper()
	entireDir := filepath.Join(dir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	settings := `{"enabled": true, "strategy": "manual-commit", "commit_linking": "always"}`
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

// Sample file lists for varied FilesTouched per session (used by IDLE/ACTIVE
// which need actual files on disk via seedSessionWithShadowBranch).
var perfFileSets = [][]string{
	{"main.go", "go.mod"},
	{"cmd/entire/main.go", "cmd/entire/cli/root.go"},
	{"go.sum", "README.md", "Makefile"},
	{"cmd/entire/cli/strategy/common.go"},
	{"cmd/entire/cli/session/state.go", "cmd/entire/cli/session/phase.go"},
	{"cmd/entire/cli/paths/paths.go", "cmd/entire/cli/paths/worktree.go", "go.mod"},
	{"cmd/entire/cli/agent/claude.go"},
	{"docs/architecture/README.md", "CLAUDE.md"},
}

// perfLargeFileSets provides realistic file path lists matching production
// session sizes (30-80 files). Real sessions have 30-350+ files touched.
// Each set includes "perf_control.txt" so PrepareCommitMsg's staged-file
// overlap detection finds a match between staged files and FilesTouched.
var perfLargeFileSets = func() [][]string {
	dirs := []string{
		"cmd/entire/cli/strategy",
		"cmd/entire/cli/session",
		"cmd/entire/cli/checkpoint",
		"cmd/entire/cli/agent/claudecode",
		"cmd/entire/cli/agent/geminicli",
		"cmd/entire/cli/paths",
		"cmd/entire/cli/logging",
		"cmd/entire/cli/settings",
		"cmd/entire/cli",
		"docs/architecture",
	}
	var sets [][]string
	for setIdx := range 8 {
		size := 30 + (setIdx * 7) // 30, 37, 44, 51, 58, 65, 72, 79
		files := []string{"perf_control.txt"}
		for i := range size {
			dir := dirs[i%len(dirs)]
			suffix := ""
			if i%3 == 1 {
				suffix = "_test"
			}
			files = append(files, fmt.Sprintf("%s/gen_%d%s.go", dir, i, suffix))
		}
		sets = append(sets, files)
	}
	return sets
}()

// Sample prompts for varied FirstPrompt per session.
var perfPrompts = []string{
	"implement the login feature",
	"fix the bug in checkout flow",
	"refactor the session management",
	"add unit tests for the strategy package",
	"update the documentation for hooks",
	"optimize the database queries",
	"add dark mode support",
	"migrate to the new API version",
	"fix the memory leak in the worker pool",
	"add retry logic for failed API calls",
	"implement webhook support",
	"clean up unused imports and dead code",
}

// seedHookPerfSessions creates fully unique session state files.
// Each session gets a unique base commit (from repo history), varied FilesTouched,
// and unique prompts — avoiding template duplication artifacts.
//
// Phase distribution matches real-world observations from .git/entire-sessions/:
//
//	ENDED sessions (75%): shadow branch ref + data, NO LastCheckpointID.
//	    These exercise the expensive hot path: ref lookup → commit → tree →
//	    transcript/overlap check → condensation during PostCommit.
//	ENDED sessions (25%): state file with LastCheckpointID (already committed, cheap).
//	IDLE sessions:  state file + shadow branch checkpoint via SaveStep.
//	ACTIVE sessions: state file + shadow branch + live transcript file.
func seedHookPerfSessions(t *testing.T, dir string, baseCommits []string, ended, idle, active int) {
	t.Helper()

	ctx := context.Background()

	headCommit := baseCommits[0] // HEAD is always first

	worktreeID, err := paths.GetWorktreeID(dir)
	if err != nil {
		t.Fatalf("worktree ID: %v", err)
	}

	stateDir := filepath.Join(dir, ".git", session.SessionStateDirName)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	store := session.NewStateStoreWithDir(stateDir)

	agentTypes := []agent.AgentType{
		agent.AgentTypeClaudeCode,
		agent.AgentTypeClaudeCode,
		agent.AgentTypeClaudeCode,
		agent.AgentTypeGemini,
		agent.AgentTypeOpenCode,
	}

	s := &ManualCommitStrategy{}

	// --- Seed ENDED sessions ---
	// Real-world distribution (from .git/entire-sessions/ analysis):
	//   ~75% have shadow branches with data but no LastCheckpointID (not yet committed)
	//   ~25% have LastCheckpointID set and no shadow branch (already committed)
	//
	// The 75% exercise the expensive hot path per session:
	//   listAllSessionStates: packed-refs linear scan to resolve shadow branch ref
	//   sessionHasNewContent: ref → commit → tree → transcript/overlap check
	//   PostCommit condensation: write metadata to entire/checkpoints/v1 branch
	endedWithShadow := ended * 3 / 4
	endedWithoutShadow := ended - endedWithShadow

	var shadowCommitHash plumbing.Hash
	if endedWithShadow > 0 {
		// Create one template session via SaveStep to establish a shadow branch
		// with a commit/tree containing proper transcript data.
		templateID := "perf-ended-0"
		seedSessionWithShadowBranch(t, s, dir, templateID, session.PhaseEnded, perfFileSets[0])

		// Get the shadow branch commit hash to create alias refs.
		repo, openErr := git.PlainOpen(dir)
		if openErr != nil {
			t.Fatalf("open repo for shadow refs: %v", openErr)
		}
		shadowName := checkpoint.ShadowBranchNameForCommit(headCommit, worktreeID)
		ref, refErr := repo.Reference(plumbing.NewBranchReferenceName(shadowName), true)
		if refErr != nil {
			t.Fatalf("find template shadow branch %q: %v", shadowName, refErr)
		}
		shadowCommitHash = ref.Hash()

		// Enrich template session with realistic FilesTouched.
		tState, loadErr := s.loadSessionState(ctx, templateID)
		if loadErr != nil {
			t.Fatalf("load template state: %v", loadErr)
		}
		tState.AgentType = agentTypes[0]
		tState.FirstPrompt = perfPrompts[0]
		tState.FilesTouched = perfLargeFileSets[0]
		if saveErr := s.saveSessionState(ctx, tState); saveErr != nil {
			t.Fatalf("save template state: %v", saveErr)
		}

		// Remaining shadow-branch sessions: create alias refs + state files.
		// Each gets a unique base commit → unique shadow branch name → different
		// packed-refs lookup per session (go-git has no ref caching).
		for i := 1; i < endedWithShadow; i++ {
			sessionID := fmt.Sprintf("perf-ended-%d", i)
			baseIdx := (i + 1) % len(baseCommits)
			base := baseCommits[baseIdx]

			// Create shadow branch ref pointing to template's commit.
			// The hook code resolves this ref, gets the commit/tree, then
			// checks for transcript or FilesTouched overlap — exercising
			// the full expensive code path.
			aliasName := checkpoint.ShadowBranchNameForCommit(base, worktreeID)
			aliasRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(aliasName), shadowCommitHash)
			if setErr := repo.Storer.SetReference(aliasRef); setErr != nil {
				t.Fatalf("create shadow alias %d: %v", i, setErr)
			}

			now := time.Now()
			state := &session.State{
				SessionID:    sessionID,
				CLIVersion:   "dev",
				BaseCommit:   base,
				WorktreePath: dir,
				WorktreeID:   worktreeID,
				Phase:        session.PhaseEnded,
				StartedAt:    now.Add(-time.Duration(i+1) * time.Hour),
				// No LastCheckpointID — exercises the expensive sessionHasNewContent path
				StepCount:           (i % 5) + 1,
				FilesTouched:        perfLargeFileSets[i%len(perfLargeFileSets)],
				LastInteractionTime: &now,
				AgentType:           agentTypes[i%len(agentTypes)],
				FirstPrompt:         perfPrompts[i%len(perfPrompts)],
			}
			if saveErr := store.Save(ctx, state); saveErr != nil {
				t.Fatalf("save ended-shadow state %d: %v", i, saveErr)
			}
		}
	}

	// Already-committed ENDED sessions (25%): state file only, no shadow branch.
	// These have LastCheckpointID set — cheap path during hooks.
	for i := range endedWithoutShadow {
		idx := endedWithShadow + i
		sessionID := fmt.Sprintf("perf-ended-%d", idx)
		cpID := mustGenerateCheckpointID(t)
		now := time.Now()
		baseIdx := (idx + 1) % len(baseCommits)
		base := baseCommits[baseIdx]

		state := &session.State{
			SessionID:           sessionID,
			CLIVersion:          "dev",
			BaseCommit:          base,
			WorktreePath:        dir,
			WorktreeID:          worktreeID,
			Phase:               session.PhaseEnded,
			StartedAt:           now.Add(-time.Duration(idx+1) * time.Hour),
			LastCheckpointID:    cpID,
			StepCount:           (idx % 5) + 1,
			FilesTouched:        perfLargeFileSets[idx%len(perfLargeFileSets)],
			LastInteractionTime: &now,
			AgentType:           agentTypes[idx%len(agentTypes)],
			FirstPrompt:         perfPrompts[idx%len(perfPrompts)],
		}
		if saveErr := store.Save(ctx, state); saveErr != nil {
			t.Fatalf("save ended-committed state %d: %v", i, saveErr)
		}
	}

	// --- Seed IDLE sessions (with shadow branches) ---
	// IDLE sessions have the current HEAD as base commit (they're recent).
	for i := range idle {
		sessionID := fmt.Sprintf("perf-idle-%d", i)
		files := perfFileSets[i%len(perfFileSets)]
		seedSessionWithShadowBranch(t, s, dir, sessionID, session.PhaseIdle, files)

		// Enrich state with unique data.
		state, loadErr := s.loadSessionState(ctx, sessionID)
		if loadErr != nil {
			t.Fatalf("load idle state %d: %v", i, loadErr)
		}
		state.AgentType = agentTypes[i%len(agentTypes)]
		state.FirstPrompt = perfPrompts[i%len(perfPrompts)]
		state.StepCount = (i % 3) + 1
		if saveErr := s.saveSessionState(ctx, state); saveErr != nil {
			t.Fatalf("save idle state %d: %v", i, saveErr)
		}
	}

	// --- Seed ACTIVE sessions (shadow branch + live transcript) ---
	for i := range active {
		sessionID := fmt.Sprintf("perf-active-%d", i)
		files := perfFileSets[i%len(perfFileSets)]
		seedSessionWithShadowBranch(t, s, dir, sessionID, session.PhaseActive, files)

		// Create a live transcript file with varied content.
		claudeProjectDir := filepath.Join(dir, ".claude", "projects", "test", "sessions")
		if mkdirErr := os.MkdirAll(claudeProjectDir, 0o755); mkdirErr != nil {
			t.Fatalf("mkdir claude sessions: %v", mkdirErr)
		}
		prompt := perfPrompts[i%len(perfPrompts)]
		transcript := fmt.Sprintf(`{"type":"human","message":{"content":"%s"}}
{"type":"assistant","message":{"content":"I'll work on that for you. Let me start by examining the codebase."}}
{"type":"tool_use","name":"read","input":{"path":"%s"}}
{"type":"tool_use","name":"write","input":{"path":"%s","content":"package main\n// modified by session %d\nfunc main() {}\n"}}
`, prompt, files[0], files[0], i)
		transcriptFile := filepath.Join(claudeProjectDir, sessionID+".jsonl")
		if writeErr := os.WriteFile(transcriptFile, []byte(transcript), 0o644); writeErr != nil {
			t.Fatalf("write live transcript: %v", writeErr)
		}

		state, loadErr := s.loadSessionState(ctx, sessionID)
		if loadErr != nil {
			t.Fatalf("load active state %d: %v", i, loadErr)
		}
		state.AgentType = agentTypes[i%len(agentTypes)]
		state.FirstPrompt = prompt
		state.TranscriptPath = transcriptFile
		if saveErr := s.saveSessionState(ctx, state); saveErr != nil {
			t.Fatalf("save active state %d: %v", i, saveErr)
		}
	}

	// Count unique base commits actually used.
	seen := make(map[string]struct{})
	states, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list states: %v", err)
	}
	for _, st := range states {
		seen[st.BaseCommit] = struct{}{}
	}

	t.Logf("  Seeded %d sessions (ended=%d [%d shadow, %d committed], idle=%d, active=%d), %d unique base commits",
		len(states), ended, endedWithShadow, endedWithoutShadow, idle, active, len(seen))
}

// seedSessionWithShadowBranch creates a session with a shadow branch checkpoint
// using SaveStep, then sets the desired phase.
func seedSessionWithShadowBranch(t *testing.T, s *ManualCommitStrategy, dir, sessionID string, phase session.Phase, modifiedFiles []string) {
	t.Helper()
	ctx := context.Background()

	for _, f := range modifiedFiles {
		abs := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", f, err)
		}
		content := fmt.Sprintf("package main\n// modified by agent %s\nfunc f() {}\n", sessionID)
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o755); err != nil {
		t.Fatalf("mkdir metadata: %v", err)
	}
	transcript := `{"type":"human","message":{"content":"implement feature"}}
{"type":"assistant","message":{"content":"I'll implement that for you."}}
`
	if err := os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	paths.ClearWorktreeRootCache()

	if err := s.SaveStep(ctx, StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  modifiedFiles,
		NewFiles:       []string{},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Perf",
		AuthorEmail:    "perf@test.com",
	}); err != nil {
		t.Fatalf("SaveStep %s: %v", sessionID, err)
	}

	state, err := s.loadSessionState(ctx, sessionID)
	if err != nil {
		t.Fatalf("load state %s: %v", sessionID, err)
	}
	state.Phase = phase
	state.FilesTouched = modifiedFiles
	if err := s.saveSessionState(ctx, state); err != nil {
		t.Fatalf("save state %s: %v", sessionID, err)
	}
}

func mustGenerateCheckpointID(t *testing.T) id.CheckpointID {
	t.Helper()
	cpID, err := id.Generate()
	if err != nil {
		t.Fatalf("generate checkpoint ID: %v", err)
	}
	return cpID
}
