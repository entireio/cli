package strategy

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

func TestPostRewrite_AmendCommitLinks(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		args          []string
		wantLink      bool
		rewriteType   string
		wrongTarget   bool
		normalCommit  bool
		multi         bool
	}{
		{name: "message", message: "old\n\nEntire-Checkpoint: abc123def456", args: []string{"-m", "renamed"}, wantLink: true},
		{name: "file", message: "old\n\nEntire-Checkpoint: abc123def456", args: []string{"-F", "message.txt"}, wantLink: true},
		{name: "no edit", message: "old\n\nEntire-Checkpoint: abc123def456", args: []string{"--no-edit"}},
		{name: "body forgery", message: "old\n\nEntire-Checkpoint: abc123def456\n\nordinary body", args: []string{"-m", "renamed"}},
		{name: "multiple trailers", message: "old\n\nEntire-Checkpoint: abc123def456\nEntire-Checkpoint: 111111222222", args: []string{"-m", "renamed"}, wantLink: true, multi: true},
		{name: "missing checkpoint does not block valid links", message: "old\n\nEntire-Checkpoint: deadbeefcafe\nEntire-Checkpoint: abc123def456", args: []string{"-m", "renamed"}, wantLink: true},
		{name: "rebase", message: "old\n\nEntire-Checkpoint: abc123def456", args: []string{"-m", "renamed"}, rewriteType: "rebase"},
		{name: "wrong destination", message: "old\n\nEntire-Checkpoint: abc123def456", args: []string{"-m", "renamed"}, wrongTarget: true},
		{name: "different parents", message: "old\n\nEntire-Checkpoint: abc123def456", args: []string{"--allow-empty", "-m", "child"}, normalCommit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// CWD-based production hooks require process-global isolation.
			testutil.IsolateGitConfigEnv(t)
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			testutil.WriteFile(t, dir, "f", "base")
			testutil.GitAdd(t, dir, "f")
			testutil.GitCommit(t, dir, tc.message)
			t.Chdir(dir)
			ctx := context.Background()
			repo, err := OpenRepository(ctx)
			require.NoError(t, err)
			defer repo.Close()
			store := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs())
			cid := id.MustCheckpointID("abc123def456")
			require.NoError(t, store.Write(ctx, checkpoint.Session{CheckpointID: cid, SessionID: "s1", Transcript: redact.AlreadyRedacted([]byte("original")), AuthorName: "Test", AuthorEmail: "test@example.com"}))
			secondID := id.MustCheckpointID("111111222222")
			if tc.multi {
				require.NoError(t, store.Write(ctx, checkpoint.Session{CheckpointID: secondID, SessionID: "s2", Transcript: redact.AlreadyRedacted([]byte("second")), AuthorName: "Test", AuthorEmail: "test@example.com"}))
			}
			old := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
			testutil.WriteFile(t, dir, "message.txt", "renamed from file\n")
			baseArgs := []string{"commit", "--amend", "--no-gpg-sign"}
			if tc.normalCommit {
				baseArgs = []string{"commit", "--no-gpg-sign"}
			}
			baseArgs = append(baseArgs, tc.args...)
			testutil.RunGit(t, dir, baseArgs...)
			newSHA := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
			strategy := NewManualCommitStrategy()
			rewriteType := tc.rewriteType
			if rewriteType == "" {
				rewriteType = "amend"
			}
			target := newSHA
			if tc.wrongTarget {
				target = old
			}
			require.NoError(t, strategy.PostRewrite(ctx, rewriteType, strings.NewReader(old+" "+target+"\n")))
			summary, err := store.Read(ctx, cid)
			require.NoError(t, err)
			if !tc.wantLink {
				require.Empty(t, summary.LinkedCommits)
				return
			}
			require.Equal(t, []checkpoint.LinkedCommit{{SHA: newSHA}}, summary.LinkedCommits)
			if tc.multi {
				second, readErr := store.Read(ctx, secondID)
				require.NoError(t, readErr)
				require.Equal(t, summary.LinkedCommits, second.LinkedCommits)
			}
			// Replaying the hook does not duplicate links, and another amend
			// carries stored links when the replaced commit has no trailer.
			require.NoError(t, strategy.PostRewrite(ctx, "amend", strings.NewReader(old+" "+newSHA+"\n")))
			testutil.RunGit(t, dir, "commit", "--amend", "-m", "renamed again")
			latest := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
			require.NoError(t, strategy.PostRewrite(ctx, "amend", strings.NewReader(newSHA+" "+latest+"\n")))
			summary, err = store.Read(ctx, cid)
			require.NoError(t, err)
			require.Equal(t, []checkpoint.LinkedCommit{{SHA: newSHA}, {SHA: latest}}, summary.LinkedCommits)
			require.Equal(t, latest, strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD")), "repair must never rewrite the code commit")
		})
	}
}
