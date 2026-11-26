//go:build integration && (linux || darwin)

package integration

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Cross-repository linking: with global enable on, a session started in
// repository C links the commits its agent makes in any other repository D.
// Every repository lives under one HOME (Entire links only repositories
// there), and they share one user config directory. The recorded session
// owner is the test binary, an ancestor of every hook it runs, which is what
// an agent's own commits look like.

type crossRepo struct {
	c         *TestEnv
	others    []*TestEnv
	configDir string
	extra     []string
}

func newCrossRepo(t *testing.T, others int) *crossRepo {
	t.Helper()
	if _, ok := proclive.IdentityOf(os.Getpid()); !ok {
		t.Skip("process ancestry is not supported on this platform")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	extra, _ := userTierEnv(t, `{"global_enable": true}`)
	extra = append(extra, "HOME="+home)
	x := &crossRepo{extra: extra}
	for _, kv := range extra {
		if v, ok := strings.CutPrefix(kv, "ENTIRE_CONFIG_DIR="); ok {
			x.configDir = v
		}
	}
	x.c = x.newRepo(t, filepath.Join(home, "c"))
	for i := range others {
		x.others = append(x.others, x.newRepo(t, filepath.Join(home, "d"+string(rune('1'+i)))))
	}
	return x
}

// newRepo creates a repository with one commit on a feature branch at dir.
func (x *crossRepo) newRepo(t *testing.T, dir string) *TestEnv {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := envAt(t, dir, x.extra)
	env.InitRepo()
	env.WriteFile("README.md", "# "+filepath.Base(dir))
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/cross")
	return env
}

// envAt is NewTestEnv for an existing directory.
func envAt(t *testing.T, dir string, extra []string) *TestEnv {
	t.Helper()
	env := NewTestEnv(t)
	env.RepoDir = dir
	env.ExtraEnv = extra
	return env
}

// startTurn submits a prompt to the session in C.
func (x *crossRepo) startTurn(t *testing.T, sess *Session, prompt string) {
	t.Helper()
	if err := x.c.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath); err != nil {
		t.Fatalf("user-prompt-submit: %v", err)
	}
}

// edit writes file in d as the session's agent and records the edit in its
// transcript, by absolute path.
func edit(t *testing.T, sess *Session, d *TestEnv, file string) {
	t.Helper()
	d.WriteFile(file, "from the agent: "+file)
	sess.CreateTranscript("edit "+file, []FileChange{{Path: filepath.Join(d.RepoDir, file), Content: "from the agent: " + file}})
}

func (x *crossRepo) endTurn(t *testing.T, sess *Session) {
	t.Helper()
	if err := x.c.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// installByTurn runs one turn that edits a file in every other repository, so
// its end installs Entire's git hooks there, and starts the next turn.
func (x *crossRepo) installByTurn(t *testing.T) *Session {
	t.Helper()
	sess := x.c.NewSession()
	x.startTurn(t, sess, "edit the other repositories")
	for _, d := range x.others {
		edit(t, sess, d, "first.txt")
	}
	x.endTurn(t, sess)
	for _, d := range x.others {
		if !strategy.IsGitHookInstalledInDir(context.Background(), d.RepoDir) {
			t.Fatalf("turn end did not install Entire's git hooks in %s", d.RepoDir)
		}
	}
	x.startTurn(t, sess, "commit there")
	return sess
}

// checkpointFor returns d's stored checkpoint cpID, failing when it is missing.
func checkpointFor(t *testing.T, d *TestEnv, cpID string) checkpoint.CheckpointInfo {
	t.Helper()
	for _, info := range listStoredCheckpoints(t, d) {
		if info.CheckpointID.String() == cpID {
			return info
		}
	}
	t.Fatalf("checkpoint %s not stored in %s", cpID, d.RepoDir)
	return checkpoint.CheckpointInfo{}
}

// assertAgentCommitLinked commits file in d as the agent and checks the
// trailer names a marker checkpoint, in store's checkpoint ref, of sessionID.
// It returns the commit.
func assertAgentCommitLinked(t *testing.T, d, store *TestEnv, file, sessionID string) string {
	t.Helper()
	d.WriteFile(file, "committed by the agent")
	d.GitCommitWithHooksAsAgent("agent commit "+file, file)
	sha := d.GetHeadHash()
	cpID := d.GetCheckpointIDFromCommitMessage(sha)
	if cpID == "" {
		t.Fatalf("agent commit in %s not linked:\n%s", d.RepoDir, d.GetCommitMessage(sha))
	}
	assertMarker(t, store, cpID, sessionID, sha)
	return sha
}

// assertMarker checks checkpoint cpID in d is a marker of sessionID for sha,
// holding nothing of the session's log.
func assertMarker(t *testing.T, d *TestEnv, cpID, sessionID, sha string) {
	t.Helper()
	if got := checkpointFor(t, d, cpID).SessionID; got != sessionID {
		t.Errorf("checkpoint %s carries session %q, want %q", cpID, got, sessionID)
	}
	meta := readSessionMetadata(t, d, cpID)
	if meta.CrossRepo == nil || meta.CrossRepo.CommitSHA != sha || meta.CrossRepo.HomeRepo == "" {
		t.Errorf("checkpoint %s is not a marker for %s: %+v", cpID, sha, meta.CrossRepo)
	}
	if meta.TokenUsage != nil {
		t.Errorf("marker %s carries token usage", cpID)
	}
	if transcript, found := d.ReadFileFromBranch(paths.MetadataBranchName, strings.TrimSuffix(SessionMetadataPath(cpID), "metadata.json")+"full.jsonl"); found && transcript != "" {
		t.Errorf("marker %s stores a transcript", cpID)
	}
}

// assertHomeCheckpointLists commits in C as the agent and checks that
// checkpoint lists shas among the session's cross-repository commits.
func (x *crossRepo) assertHomeCheckpointLists(t *testing.T, shas ...string) {
	t.Helper()
	x.c.WriteFile("home.txt", "home work "+shas[0])
	x.c.GitCommitWithHooksAsAgent("home commit", "home.txt")
	cpID := x.c.GetCheckpointIDFromCommitMessage(x.c.GetHeadHash())
	if cpID == "" {
		t.Fatal("home commit not linked")
	}
	meta := readSessionMetadata(t, x.c, cpID)
	for _, sha := range shas {
		if !slices.ContainsFunc(meta.LinkedRepos, func(l checkpoint.LinkedRepoCommit) bool { return l.SHA == sha }) {
			t.Errorf("home checkpoint %s does not list %s: %+v", cpID, sha, meta.LinkedRepos)
		}
	}
	if out := x.c.RunCLI("checkpoint", "explain", cpID); !strings.Contains(out, shas[0]) {
		t.Errorf("checkpoint explain does not list %s:\n%s", shas[0], out)
	}
	if len(meta.SessionLinkedRepos) < len(shas) {
		t.Errorf("session-level list %+v shorter than %d", meta.SessionLinkedRepos, len(shas))
	}
}

func TestCrossRepo_AgentCommitAfterLazyInstallLinks(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 1)
	d := x.others[0]
	sess := x.installByTurn(t)

	inD := assertAgentCommitLinked(t, d, d, "second.txt", sess.ID)
	if n := len(listStoredCheckpoints(t, x.c)); n != 0 {
		t.Errorf("C stored %d checkpoint(s) for D's commit", n)
	}

	// A worktree of D shares its hooks and its checkpoint ref.
	wt := filepath.Join(filepath.Dir(d.RepoDir), "d-wt")
	testutil.RunGit(t, d.RepoDir, "worktree", "add", "-b", "d-wt", wt)
	inWorktree := assertAgentCommitLinked(t, envAt(t, wt, x.extra), d, "wt.txt", sess.ID)
	x.assertHomeCheckpointLists(t, inD, inWorktree)
}

func TestCrossRepo_CommitBeforeInstallGetsRecordedLink(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 1)
	d := x.others[0]
	sess := x.c.NewSession()
	x.startTurn(t, sess, "edit and commit in d")
	edit(t, sess, d, "early.txt")
	d.GitAdd("early.txt")
	d.GitCommit("committed before Entire's hooks were there")
	sha := d.GetHeadHash()
	x.endTurn(t, sess)

	infos := listStoredCheckpoints(t, d)
	if len(infos) != 1 || infos[0].SessionID != sess.ID || !slices.ContainsFunc(infos[0].LinkedCommits, func(l checkpoint.LinkedCommit) bool { return l.SHA == sha }) {
		t.Fatalf("want one checkpoint of %s recording a link to %s, got %+v", sess.ID, sha, infos)
	}
	if d.GetCheckpointIDFromCommitMessage(sha) != "" {
		t.Error("a recorded link must not amend the commit")
	}
	assertMarker(t, d, infos[0].CheckpointID.String(), sess.ID, sha)

	// Idempotent: a later turn end does not link it again.
	x.startTurn(t, sess, "again")
	edit(t, sess, d, "early.txt")
	x.endTurn(t, sess)
	if n := len(listStoredCheckpoints(t, d)); n != 1 {
		t.Errorf("the commit was linked again: %d checkpoints", n)
	}
}

func TestCrossRepo_SubagentCommitLinks(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 1)
	sess := x.installByTurn(t)
	if err := x.c.SimulatePreTask(sess.ID, sess.TranscriptPath, "toolu_cross"); err != nil {
		t.Fatalf("pre-task: %v", err)
	}
	assertAgentCommitLinked(t, x.others[0], x.others[0], "sub.txt", sess.ID)
}

func TestCrossRepo_TwoOtherRepositories(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 2)
	sess := x.installByTurn(t)
	var shas []string
	for _, d := range x.others {
		shas = append(shas, assertAgentCommitLinked(t, d, d, "second.txt", sess.ID))
	}
	x.assertHomeCheckpointLists(t, shas...)
}

func TestCrossRepo_HumanCommitStaysUnlinked(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 1)
	d := x.others[0]
	sess := x.installByTurn(t)
	edit(t, sess, d, "agent.txt")
	// A commit whose process tree holds no agent finds no session pointer.
	if err := os.RemoveAll(filepath.Join(x.configDir, "session-owners")); err != nil {
		t.Fatal(err)
	}
	d.WriteFile("human.txt", "typed by a person")
	d.GitCommitWithHooks("human commit", "human.txt")
	x.endTurn(t, sess)

	if cpID := d.GetCheckpointIDFromCommitMessage(d.GetHeadHash()); cpID != "" {
		t.Errorf("human commit linked to %s", cpID)
	}
	if n := len(listStoredCheckpoints(t, d)); n != 0 {
		t.Errorf("D stored %d checkpoint(s) for a human commit", n)
	}
}

func TestCrossRepo_DisabledRepositoryIsSkipped(t *testing.T) {
	t.Parallel()
	for name, disable := range map[string]func(d *TestEnv){
		"committed enabled false": func(d *TestEnv) {
			d.WriteFile(".entire/settings.json", `{"enabled": false}`)
			d.GitAdd(".entire/settings.json")
			d.GitCommit("Disable Entire")
		},
		"entire disable": func(d *TestEnv) { d.RunCLI("disable") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			x := newCrossRepo(t, 1)
			d := x.others[0]
			disable(d)
			sess := x.c.NewSession()
			x.startTurn(t, sess, "edit d")
			edit(t, sess, d, "skip.txt")
			x.endTurn(t, sess)
			if strategy.IsGitHookInstalledInDir(context.Background(), d.RepoDir) {
				t.Error("Entire installed its git hooks in a disabled repository")
			}
			x.startTurn(t, sess, "commit")
			d.GitCommitWithHooksAsAgent("agent commit", "skip.txt")
			if cpID := d.GetCheckpointIDFromCommitMessage(d.GetHeadHash()); cpID != "" {
				t.Errorf("commit in a disabled repository linked to %s", cpID)
			}
		})
	}
}

func TestCrossRepo_StatusAndDisableGlobal(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 1)
	sess := x.installByTurn(t)
	d := x.others[0]
	assertAgentCommitLinked(t, d, d, "status.txt", sess.ID)
	if out := d.RunCLI("status"); !strings.Contains(out, "link to session "+sess.ID) {
		t.Errorf("status in D does not name the session:\n%s", out)
	}
	if out := x.c.RunCLI("status"); !strings.Contains(out, "committed in 1 other repositories") {
		t.Errorf("status in C does not count D:\n%s", out)
	}
	pointers := filepath.Join(x.configDir, "session-owners")
	if _, err := os.Stat(pointers); err != nil {
		t.Fatalf("no session pointers recorded: %v", err)
	}

	out := x.c.RunCLI("disable", "--global")
	if !strings.Contains(out, x.others[0].RepoDir) {
		t.Errorf("disable --global did not list %s:\n%s", x.others[0].RepoDir, out)
	}
	if _, err := os.Stat(pointers); !os.IsNotExist(err) {
		t.Errorf("session pointers left behind: %v", err)
	}
}
