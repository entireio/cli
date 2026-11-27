//go:build integration && (linux || darwin)

package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
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
	if meta.CrossRepo != nil && (filepath.IsAbs(meta.CrossRepo.HomeRepo) || filepath.IsAbs(meta.CrossRepo.Repo)) {
		t.Errorf("marker %s names a repository by local path: %+v", cpID, meta.CrossRepo)
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
	for _, l := range meta.SessionLinkedRepos {
		if l.RepoKey == "" || filepath.IsAbs(l.RepoKey) {
			t.Errorf("home checkpoint %s names a repository by local path: %+v", cpID, l)
		}
	}
	var order []string
	for _, l := range meta.SessionLinkedRepos {
		if slices.Contains(shas, l.SHA) {
			order = append(order, l.SHA)
		}
	}
	if !slices.Equal(order, shas) {
		t.Errorf("session-level list orders %v, want commit order %v", order, shas)
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

// newPlainRepo creates, at dir, a repository as `git init` makes one: a main
// worktree with git's sample hooks and no core.hooksPath (InitRepo sets an
// absolute one, which hides how git answers for an ordinary repository).
func (x *crossRepo) newPlainRepo(t *testing.T, dir string) *TestEnv {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, dir, "init", "-q")
	testutil.RunGit(t, dir, "config", "user.name", testAuthorName)
	testutil.RunGit(t, dir, "config", "user.email", testAuthorEmail)
	testutil.RunGit(t, dir, "config", "commit.gpgsign", "false")
	env := envAt(t, dir, x.extra)
	env.setGitConfigBaseline()
	env.WriteFile("README.md", "# "+filepath.Base(dir))
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/cross")
	return env
}

// commitUnhooked commits file in d without running git hooks: a commit made
// before Entire's hooks were there.
func commitUnhooked(d *TestEnv, file string) string {
	d.T.Helper()
	return commitUnhookedAt(d, file, time.Now())
}

// commitUnhookedAt is commitUnhooked with an explicit commit time.
func commitUnhookedAt(d *TestEnv, file string, when time.Time) string {
	d.T.Helper()
	d.WriteFile(file, "committed: "+file)
	d.GitAdd(file)
	d.GitCommitAt("commit "+file, when)
	return d.GetHeadHash()
}

// assertRecordedLink checks d stores a marker of sessionID recording a link
// to sha.
func assertRecordedLink(t *testing.T, d *TestEnv, sessionID, sha string) {
	t.Helper()
	for _, info := range listStoredCheckpoints(t, d) {
		if info.SessionID == sessionID && slices.ContainsFunc(info.LinkedCommits, func(l checkpoint.LinkedCommit) bool { return l.SHA == sha }) {
			assertMarker(t, d, info.CheckpointID.String(), sessionID, sha)
			return
		}
	}
	t.Fatalf("%s holds no recorded link of %s to %s: %+v", d.RepoDir, sessionID, sha, listStoredCheckpoints(t, d))
}

func assertNoRecordedLink(t *testing.T, d *TestEnv, sha string) {
	t.Helper()
	for _, info := range listStoredCheckpoints(t, d) {
		if slices.ContainsFunc(info.LinkedCommits, func(l checkpoint.LinkedCommit) bool { return l.SHA == sha }) {
			t.Fatalf("%s linked %s, which the agent neither reported nor touched", d.RepoDir, sha)
		}
	}
}

func TestCrossRepo_MainWorktreeWithoutHooksPath(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 0)
	d := x.newPlainRepo(t, filepath.Join(filepath.Dir(x.c.RepoDir), "plain"))
	sess := x.c.NewSession()
	x.startTurn(t, sess, "edit and commit in d")
	edit(t, sess, d, "d1.txt")
	d1 := commitUnhooked(d, "d1.txt")
	x.endTurn(t, sess)

	if !strategy.IsGitHookInstalledInDir(context.Background(), d.RepoDir) {
		t.Fatal("turn end did not install Entire's git hooks in an ordinary repository")
	}
	assertRecordedLink(t, d, sess.ID, d1)
	x.startTurn(t, sess, "commit again")
	assertAgentCommitLinked(t, d, d, "d2.txt", sess.ID)
}

func TestCrossRepo_SweepLinksWhenTheInstallIsBlocked(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 0)
	d := x.newPlainRepo(t, filepath.Join(filepath.Dir(x.c.RepoDir), "shared-hooks"))
	testutil.RunGit(t, d.RepoDir, "config", "core.hooksPath", t.TempDir())
	d.setGitConfigBaseline()
	sess := x.c.NewSession()
	x.startTurn(t, sess, "edit and commit in d")
	edit(t, sess, d, "blocked.txt")
	sha := commitUnhooked(d, "blocked.txt")
	x.endTurn(t, sess)

	if strategy.IsGitHookInstalledInDir(context.Background(), d.RepoDir) {
		t.Fatal("Entire installed its hooks into a shared hooks directory")
	}
	assertRecordedLink(t, d, sess.ID, sha)
}

func TestCrossRepo_SubagentEditsAndCommitsLink(t *testing.T) {
	t.Parallel()
	t.Run("at subagent stop", func(t *testing.T) {
		t.Parallel()
		x := newCrossRepo(t, 0)
		e := x.newPlainRepo(t, filepath.Join(filepath.Dir(x.c.RepoDir), "e"))
		sess := x.c.NewSession()
		x.startTurn(t, sess, "delegate")
		e.WriteFile("sub.txt", "by the subagent")
		sha := commitUnhooked(e, "sub.txt")
		sub := sess.CreateSubagentTranscript("agentsub1", []FileChange{{Path: filepath.Join(e.RepoDir, "sub.txt"), Content: "by the subagent"}})
		if err := x.c.SimulateSubagentStop(SubagentStopInput{SessionID: sess.ID, TranscriptPath: sess.TranscriptPath, AgentID: "agentsub1", AgentTranscriptPath: sub}); err != nil {
			t.Fatalf("subagent-stop: %v", err)
		}
		assertRecordedLink(t, e, sess.ID, sha)
	})
	t.Run("at the parent's turn end", func(t *testing.T) {
		t.Parallel()
		x := newCrossRepo(t, 0)
		e := x.newPlainRepo(t, filepath.Join(filepath.Dir(x.c.RepoDir), "e"))
		sess := x.c.NewSession()
		x.startTurn(t, sess, "delegate")
		e.WriteFile("sub.txt", "by the subagent")
		sha := commitUnhooked(e, "sub.txt")
		sess.CreateSubagentTranscript("agentsub2", []FileChange{{Path: filepath.Join(e.RepoDir, "sub.txt"), Content: "by the subagent"}})
		sess.TranscriptBuilder.AddUserMessage("delegate")
		taskID := sess.TranscriptBuilder.AddTaskToolUse("", "edit e")
		sess.TranscriptBuilder.AddTaskToolResult(taskID, "agentsub2")
		if err := sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath); err != nil {
			t.Fatal(err)
		}
		x.endTurn(t, sess)
		if !strategy.IsGitHookInstalledInDir(context.Background(), e.RepoDir) {
			t.Error("turn end did not install Entire's git hooks where the subagent edited")
		}
		assertRecordedLink(t, e, sess.ID, sha)
	})
}

func TestCrossRepo_ShellOnlyWorkLinks(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 0)
	d := x.newPlainRepo(t, filepath.Join(filepath.Dir(x.c.RepoDir), "shell"))
	sess := x.c.NewSession()
	x.startTurn(t, sess, "work in d from the shell")
	human := commitUnhooked(d, "human.txt")
	agent := commitUnhooked(d, "shell.txt")
	sess.TranscriptBuilder.AddUserMessage("work in d from the shell")
	sess.TranscriptBuilder.AddBashToolUse(
		"cd "+d.RepoDir+" && echo hi > "+filepath.Join(d.RepoDir, "shell.txt")+" && git add -A && git commit -m 'commit shell.txt'",
		"[feature/cross "+agent[:7]+"] commit shell.txt\n 1 file changed, 1 insertion(+)")
	if err := sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	x.endTurn(t, sess)

	if !strategy.IsGitHookInstalledInDir(context.Background(), d.RepoDir) {
		t.Error("turn end did not install Entire's git hooks where the shell worked")
	}
	assertRecordedLink(t, d, sess.ID, agent)
	assertNoRecordedLink(t, d, human)
}

func TestCrossRepo_PiAbsolutePathWrite(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 0)
	d := x.newPlainRepo(t, filepath.Join(filepath.Dir(x.c.RepoDir), "pi-d"))
	const sessionID = "01a21084-8b1e-7096-adef-2b7067b61e6f"
	sessionFile := filepath.Join(t.TempDir(), "2026-11-25T02-24-11-552Z_"+sessionID+".jsonl")
	header := `{"type":"session","version":3,"id":"` + sessionID + `","cwd":"` + x.c.RepoDir + `"}` + "\n"
	if err := os.WriteFile(sessionFile, []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := x.c.newHookRunner()
	piHook := func(hook string, payload map[string]any) {
		t.Helper()
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if out := runner.runAgentHookWithOutput("pi", hook, data); out.Err != nil {
			t.Fatalf("pi %s: %v\n%s", hook, out.Err, out.Stderr)
		}
	}
	piHook("before_agent_start", map[string]any{"type": "before_agent_start", "cwd": x.c.RepoDir, "session_file": sessionFile, "session_id": sessionID, "prompt": "write d"})

	target := filepath.Join(d.RepoDir, "pi.txt")
	d.WriteFile("pi.txt", "from pi")
	sha := commitUnhooked(d, "pi.txt")
	turn := `{"type":"message","id":"m1","parentId":null,"message":{"role":"user","content":[{"type":"text","text":"write d"}]}}
{"type":"message","id":"m2","parentId":"m1","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"write","arguments":{"path":"` + target + `","content":"from pi"}}]}}
{"type":"message","id":"m3","parentId":"m2","message":{"role":"toolResult","toolCallId":"t1","toolName":"write","content":[{"type":"text","text":"ok"}]}}
`
	f, err := os.OpenFile(sessionFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(turn); err != nil {
		t.Fatal(err)
	}
	f.Close()
	piHook("agent_end", map[string]any{"type": "agent_end", "cwd": x.c.RepoDir, "session_file": sessionFile, "session_id": sessionID})

	if !strategy.IsGitHookInstalledInDir(context.Background(), d.RepoDir) {
		t.Error("Pi's turn end did not install Entire's git hooks where it wrote")
	}
	assertRecordedLink(t, d, sessionID, sha)
}

func TestCrossRepo_StatusAfterTheAgentExits(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 1)
	d := x.others[0]
	sess := x.installByTurn(t)
	assertAgentCommitLinked(t, d, d, "linked.txt", sess.ID)
	if err := os.RemoveAll(filepath.Join(x.configDir, "session-owners")); err != nil {
		t.Fatal(err)
	}
	if out := d.RunCLI("status"); !strings.Contains(out, "linked to session "+sess.ID) {
		t.Errorf("status in D forgot the session once its agent was gone:\n%s", out)
	}
}

func TestCrossRepo_DisableGlobalNamesTheRepositoryNotTheWorktree(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 1)
	d := x.others[0]
	wt := filepath.Join(filepath.Dir(d.RepoDir), "dwt")
	testutil.RunGit(t, d.RepoDir, "worktree", "add", "-q", "-b", "dwt", wt)
	sess := x.c.NewSession()
	x.startTurn(t, sess, "edit the worktree")
	edit(t, sess, envAt(t, wt, x.extra), "wt.txt")
	x.endTurn(t, sess)

	out := x.c.RunCLI("disable", "--global")
	if !strings.Contains(out, "  "+d.RepoDir+"\n") {
		t.Errorf("disable --global does not name the repository %s:\n%s", d.RepoDir, out)
	}
	if strings.Contains(out, wt) {
		t.Errorf("disable --global names the worktree %s:\n%s", wt, out)
	}
}

// On a session's first turn the transcript starts empty, and C's same-turn
// commit condenses past the agent's edit in D: the turn-end sweep must still
// read the turn from its start and link D's commit.
func TestCrossRepo_FirstTurnSweepLinksAfterHomeCommit(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 0)
	d := x.newPlainRepo(t, filepath.Join(filepath.Dir(x.c.RepoDir), "first"))
	sess := x.c.NewSession()
	x.startTurn(t, sess, "commit in d, then in c")
	edit(t, sess, d, "d1.txt")
	d1 := commitUnhooked(d, "d1.txt")
	x.c.WriteFile("home.txt", "home work")
	x.c.GitCommitWithHooksAsAgent("home commit", "home.txt")
	x.endTurn(t, sess)

	assertRecordedLink(t, d, sess.ID, d1)
}

// linkedWorktree adds a linked worktree of d at a sibling directory named
// name, with the environment git gives commit hooks there: GIT_DIR names the
// worktree's own git directory (git exports it only in linked worktrees), and
// the agent's session ID, which Claude Code publishes to its commands.
func (x *crossRepo) linkedWorktree(t *testing.T, d *TestEnv, name, sessionID string) *TestEnv {
	t.Helper()
	wt := filepath.Join(filepath.Dir(d.RepoDir), name)
	testutil.RunGit(t, d.RepoDir, "worktree", "add", "-q", "-b", name, wt)
	gitDir := strings.TrimSpace(testutil.RunGit(t, wt, "rev-parse", "--absolute-git-dir"))
	return envAt(t, wt, append(slices.Clone(x.extra), "GIT_DIR="+gitDir, "CLAUDE_CODE_SESSION_ID="+sessionID))
}

// In a linked worktree of an already hooked D, the agent's commits link as
// in D's main checkout, and C lists them in commit order.
func TestCrossRepo_LinkedWorktreeOfHookedRepoLinks(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 1)
	d := x.others[0]
	sess := x.installByTurn(t)
	dwt := x.linkedWorktree(t, d, "dwt", sess.ID)

	first := assertAgentCommitLinked(t, dwt, d, "wt1.txt", sess.ID)
	second := assertAgentCommitLinked(t, dwt, d, "wt2.txt", sess.ID)
	x.assertHomeCheckpointLists(t, first, second)
}

// A commit in a linked worktree of D, made without hooks, changing a path the
// agent edited in D's main checkout, is linked by the turn-end sweep there:
// it scans every worktree's HEAD, not only the main checkout's.
func TestCrossRepo_SweepLinksLinkedWorktreeCommit(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 1)
	d := x.others[0]
	sess := x.c.NewSession()
	dwt := x.linkedWorktree(t, d, "dwt", sess.ID)
	x.startTurn(t, sess, "edit d, commit in its worktree")
	edit(t, sess, d, "shared.txt")
	sha := commitUnhooked(dwt, "shared.txt")
	x.endTurn(t, sess)

	assertRecordedLink(t, d, sess.ID, sha)
}

// In the first turn that commits in D, D's hooks are not installed yet, so its
// commits carry no trailer: the turn-end sweep links them after C's same-turn
// commit condensed. The session's list still follows commit order, and the
// sweep links D's commits oldest first.
func TestCrossRepo_SessionStateListsSweptCommitsInCommitOrder(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 1)
	d := x.others[0]
	sess := x.c.NewSession()
	x.startTurn(t, sess, "commit in d, then in c")

	// Explicit, increasing commit times (git's are whole seconds), after
	// the turn's start so the sweep's window includes them.
	at := time.Now().Truncate(time.Second)
	edit(t, sess, d, "d1.txt")
	d1 := commitUnhookedAt(d, "d1.txt", at.Add(time.Second))
	edit(t, sess, d, "w1.txt")
	w1 := commitUnhookedAt(d, "w1.txt", at.Add(2*time.Second))
	x.c.WriteFile("home.txt", "home work")
	x.c.GitCommitWithHooksAsAgentAt("home commit", at.Add(3*time.Second), "home.txt")
	c2 := x.c.GetHeadHash()
	x.endTurn(t, sess)

	state, err := x.c.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("load C's session state: %v", err)
	}
	var got []string
	for _, l := range state.CheckpointLinks {
		got = append(got, strings.Join(l.LinkedCommits, ","))
	}
	if want := []string{d1, w1, c2}; !slices.Equal(got, want) {
		t.Fatalf("C's list links commits %v, want commit order %v", got, want)
	}
	if state.CheckpointLinks[0].At.After(state.CheckpointLinks[1].At) {
		t.Errorf("the sweep linked w1 (at %s) before d1 (at %s)", state.CheckpointLinks[1].At, state.CheckpointLinks[0].At)
	}
	if prev := state.PreviousCheckpoint(state.CheckpointLinks[2].CheckpointID); prev != state.CheckpointLinks[1].CheckpointID {
		t.Errorf("home checkpoint's predecessor = %s, want w1's %s", prev, state.CheckpointLinks[1].CheckpointID)
	}
}

// The session's state in C lists every checkpoint it recorded, in order and
// with their commits, across D, a worktree of D and C itself, and keeps them
// across turn end and a new prompt: a home commit in the same turn as D's
// folds D's links first, and a turn with no home commit folds them at its end.
func TestCrossRepo_SessionStateListsEveryCheckpoint(t *testing.T) {
	t.Parallel()
	x := newCrossRepo(t, 1)
	d := x.others[0]
	sess := x.installByTurn(t)

	inD := assertAgentCommitLinked(t, d, d, "second.txt", sess.ID)
	wt := filepath.Join(filepath.Dir(d.RepoDir), "d-wt")
	testutil.RunGit(t, d.RepoDir, "worktree", "add", "-b", "d-wt", wt)
	inWorktree := assertAgentCommitLinked(t, envAt(t, wt, x.extra), d, "wt.txt", sess.ID)
	x.c.WriteFile("home.txt", "home work")
	x.c.GitCommitWithHooksAsAgent("home commit", "home.txt")
	home := x.c.GetHeadHash()
	x.endTurn(t, sess)

	x.startTurn(t, sess, "one more in d")
	later := assertAgentCommitLinked(t, d, d, "third.txt", sess.ID)
	x.endTurn(t, sess)
	x.startTurn(t, sess, "a new prompt clears nothing")

	state, err := x.c.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("load C's session state: %v", err)
	}
	want := []struct {
		sha  string
		kind session.CheckpointLinkKind
		cpID string
	}{
		{inD, session.CheckpointLinkCrossRepo, d.GetCheckpointIDFromCommitMessage(inD)},
		{inWorktree, session.CheckpointLinkCrossRepo, d.GetCheckpointIDFromCommitMessage(inWorktree)},
		{home, session.CheckpointLinkHome, x.c.GetCheckpointIDFromCommitMessage(home)},
		{later, session.CheckpointLinkCrossRepo, d.GetCheckpointIDFromCommitMessage(later)},
	}
	if len(state.CheckpointLinks) != len(want) {
		t.Fatalf("C's state lists %d checkpoints, want %d: %+v", len(state.CheckpointLinks), len(want), state.CheckpointLinks)
	}
	for i, w := range want {
		got := state.CheckpointLinks[i]
		if got.CheckpointID.String() != w.cpID || got.Kind != w.kind || !slices.Equal(got.LinkedCommits, []string{w.sha}) || got.TurnID == "" {
			t.Errorf("entry %d = %+v, want checkpoint %s (%s) for %s with a turn", i, got, w.cpID, w.kind, w.sha)
		}
	}
	if prev := state.PreviousCheckpoint(state.CheckpointLinks[2].CheckpointID); prev.String() != want[1].cpID {
		t.Errorf("home checkpoint's predecessor = %s, want %s", prev, want[1].cpID)
	}

	marker := readSessionMetadata(t, d, want[0].cpID)
	if !slices.Contains(marker.CrossRepo.SessionIDs, state.EntireSessionID()) || marker.TurnID == "" || marker.CrossRepo.CommitSHA != inD {
		t.Errorf("marker lacks its link fields: session_ids %v turn %q commit %q", marker.CrossRepo.SessionIDs, marker.TurnID, marker.CrossRepo.CommitSHA)
	}
	// The home commit carries the trailer: it is recorded as the commit the
	// write was for, never as a trailer-less link.
	homeMeta := readSessionMetadata(t, x.c, want[2].cpID)
	if homeMeta.TurnID == "" || homeMeta.CommitSHAAtWrite != home ||
		slices.ContainsFunc(checkpointFor(t, x.c, want[2].cpID).LinkedCommits, func(l checkpoint.LinkedCommit) bool { return l.SHA == home }) {
		t.Errorf("home checkpoint link fields: turn %q written_for_commit %q, want %s and no linked_commits entry", homeMeta.TurnID, homeMeta.CommitSHAAtWrite, home)
	}

	out := x.c.RunCLI("status", "--json")
	if !strings.Contains(out, `"entire_session_id":"`+state.EntireSessionID()+`"`) || !strings.Contains(out, want[3].cpID) {
		t.Errorf("status --json lacks the session's identity or checkpoints:\n%s", out)
	}
	// Checkpoint export describes checkpoint metadata only, the same on every
	// clone: the session's local checkpoint list is not part of it.
	if out := x.c.RunCLI("checkpoint", "explain", want[2].cpID, "--json"); strings.Contains(out, `"session_checkpoints"`) || !strings.Contains(out, `"written_for_commit": "`+home+`"`) {
		t.Errorf("checkpoint explain --json: want written_for_commit and no session_checkpoints:\n%s", out)
	}
}
