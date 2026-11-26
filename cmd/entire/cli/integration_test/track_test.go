//go:build integration && (linux || darwin)

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// The tracking rule (`entire track`, the user settings `track` block)
// narrows global enable: a denied repository, or one outside the org rule,
// behaves as not enabled, and cross-repository linking skips it.

// A repository the developer denied records nothing under global enable.
func TestTrack_DeniedRepoRecordsNothing(t *testing.T) {
	t.Parallel()
	env := newNeverEnabledRepo(t)
	extra, _ := userTierEnv(t, `{"global_enable": true, "track": {"deny": ["`+filepath.ToSlash(env.RepoDir)+`"]}}`)
	env.ExtraEnv = extra

	runClaudeTurn(t, env, "feature.go", "package feature\n")

	if cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash()); cpID != "" {
		t.Errorf("commit linked to checkpoint %s in a denied repository", cpID)
	}
	if state := strategy.CheckGitHookStateInDir(t.Context(), env.RepoDir); state != strategy.GitHooksAbsent {
		t.Errorf("Entire git hooks installed in a denied repository (state = %v)", state)
	}
	if env.FileExists(".entire/.gitignore") {
		t.Error(".entire/.gitignore written in a denied repository")
	}
}

// newOrgCrossRepo is newCrossRepo with an org rule for github.com/entireio,
// the session's repository C allowed by path (it has no origin), and D's
// origin set to origin.
func newOrgCrossRepo(t *testing.T, origin string) *crossRepo {
	t.Helper()
	x := newCrossRepo(t, 1)
	d := x.others[0]
	testutil.RunGit(t, d.RepoDir, "remote", "add", "origin", origin)
	config, err := os.ReadFile(filepath.Join(d.RepoDir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	d.AcceptGitConfigChanges(string(config))
	rule := `{"global_enable": true, "track": {"orgs": ["github.com/entireio"], "repos": ["` + filepath.ToSlash(x.c.RepoDir) + `"]}}`
	if err := os.WriteFile(filepath.Join(x.configDir, usersettings.FileName), []byte(rule), 0o600); err != nil {
		t.Fatal(err)
	}
	return x
}

// A repository outside the org rule gets no lazy install, and the agent's
// commit there links nothing.
func TestTrack_RepoOutsideTheOrgIsNotLinked(t *testing.T) {
	t.Parallel()
	x := newOrgCrossRepo(t, "https://github.com/someone-else/d.git")
	d := x.others[0]
	sess := x.c.NewSession()
	x.startTurn(t, sess, "edit d")
	edit(t, sess, d, "skip.txt")
	x.endTurn(t, sess)
	if strategy.IsGitHookInstalledInDir(context.Background(), d.RepoDir) {
		t.Error("Entire installed its git hooks in a repository outside the org rule")
	}
	x.startTurn(t, sess, "commit")
	d.GitCommitWithHooksAsAgent("agent commit", "skip.txt")
	if cpID := d.GetCheckpointIDFromCommitMessage(d.GetHeadHash()); cpID != "" {
		t.Errorf("commit in a repository outside the org rule linked to %s", cpID)
	}
}

// A repository in the org links as before.
func TestTrack_RepoInTheOrgLinks(t *testing.T) {
	t.Parallel()
	x := newOrgCrossRepo(t, "git@github.com:entireio/d.git")
	sess := x.installByTurn(t)
	assertAgentCommitLinked(t, x.others[0], x.others[0], "second.txt", sess.ID)
}
