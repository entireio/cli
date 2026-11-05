package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const (
	bodyMentionMessage = "Fix\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\nSigned-off-by: Test User <test@example.com>\n"
	semicolonMessage   = "Fix\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\n; note\n"
	commentStrMessage  = "Fix\n\nEntire-Checkpoint: a1b2c3d4e5f6\n\n// note\n"
	hashMessage        = "Fix\n\nEntire-Checkpoint: a1b2c3d4e5f6\n# note\n"
	continuedMessage   = "Fix\n\nEntire-Checkpoint: a1b2c3d4e5f6\n wrapped\nSigned-off-by: Test User <test@example.com>\nReviewed on the call\n"
)

// LinkedCheckpoints reads a saved commit's final trailer block by git's
// trailer grammar, skipping comment lines with the prefix git's config names.
// No configuration makes it read past that block.
func TestLinkedCheckpoints_FollowsGitConfig(t *testing.T) {
	mentioned := []id.CheckpointID{id.MustCheckpointID("a1b2c3d4e5f6")}
	tests := []struct {
		name    string
		config  [][2]string
		message string
		want    []id.CheckpointID
	}{
		{name: "default reads the final trailer block", message: bodyMentionMessage},
		{name: "whitespace cleanup reads the final trailer block", config: [][2]string{{"commit.cleanup", "whitespace"}}, message: bodyMentionMessage},
		{name: "verbatim cleanup reads the final trailer block", config: [][2]string{{"commit.cleanup", "verbatim"}}, message: bodyMentionMessage},
		{name: "scissors cleanup reads the final trailer block", config: [][2]string{{"commit.cleanup", "scissors"}}, message: bodyMentionMessage},
		{name: "auto reads the final trailer block", config: [][2]string{{"core.commentChar", "auto"}}, message: bodyMentionMessage},
		{name: "a continuation and a sign-off stay in the block", message: continuedMessage, want: mentioned},
		{name: "default prefix keeps ; lines", message: semicolonMessage},
		{name: "default prefix skips # lines", message: hashMessage, want: mentioned},
		{name: "configured comment char", config: [][2]string{{"core.commentChar", ";"}}, message: semicolonMessage, want: mentioned},
		{name: "configured comment char keeps # lines", config: [][2]string{{"core.commentChar", ";"}}, message: hashMessage},
		{name: "configured comment string", config: [][2]string{{"core.commentString", "//"}}, message: commentStrMessage, want: mentioned},
		// Only `git commit` resolves auto; git's trailer grammar reads it as "#".
		{name: "auto skips # lines", config: [][2]string{{"core.commentChar", "auto"}}, message: hashMessage, want: mentioned},
		{
			name:    "the last of the aliases set wins",
			config:  [][2]string{{"core.commentChar", ";"}, {"core.commentString", "//"}},
			message: semicolonMessage,
		},
		{
			name:    "the last of the aliases set wins, reversed",
			config:  [][2]string{{"core.commentString", "//"}, {"core.commentChar", ";"}},
			message: semicolonMessage,
			want:    mentioned,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testutil.IsolateGitConfigEnv(t)
			dir := resolvedTempDir(t)
			testutil.InitRepo(t, dir)
			for _, kv := range tt.config {
				testutil.RunGit(t, dir, "config", kv[0], kv[1])
			}
			t.Chdir(dir)

			got := LinkedCheckpoints(context.Background(), tt.message)
			if len(tt.want) == 0 {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

// Read-only views read without git config: the final trailer block's last ID, the
// one attach joins and --generate writes, or else the first ID anywhere.
func TestCommitCheckpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "last of the final trailer block", message: "Squash\n\nEntire-Checkpoint: aaaaaaaaaaaa\nEntire-Checkpoint: bbbbbbbbbbbb\n", want: "bbbbbbbbbbbb"},
		{name: "final trailer block over a body mention", message: "Fix\n\nEntire-Checkpoint: aaaaaaaaaaaa\n\nEntire-Checkpoint: bbbbbbbbbbbb\n", want: "bbbbbbbbbbbb"},
		{name: "first anywhere without a final-block trailer", message: "Squash\n\n    Entire-Checkpoint: aaaaaaaaaaaa\n\n    Entire-Checkpoint: bbbbbbbbbbbb\n\nBody.\n", want: "aaaaaaaaaaaa"},
		{name: "skips # comment lines", message: "Fix\n\nEntire-Checkpoint: aaaaaaaaaaaa\n\n# Please enter the commit message.\n", want: "aaaaaaaaaaaa"},
		{name: "git's trailer grammar", message: "Fix\n\nReviewed-by : A <a@example.com>\nEntire-Checkpoint: aaaaaaaaaaaa\n wrapped\n", want: "aaaaaaaaaaaa"},
		{name: "first anywhere when the last paragraph is not a trailer block", message: "Fix\n\nEntire-Checkpoint: aaaaaaaaaaaa\nprose\n", want: "aaaaaaaaaaaa"},
		{name: "none", message: "Fix\n\nBody.\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := CommitCheckpoint(tt.message)
			assert.Equal(t, tt.want != "", ok)
			assert.Equal(t, tt.want, got.String())
		})
	}
}

// A commit hook reads git's comment config once: a config changed while the
// hook runs is not picked up, as git commit itself read its config at start.
func TestGitCommentConfig_HookReadsOnce(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.RunGit(t, dir, "config", "core.commentChar", ";")
	t.Chdir(dir)
	ctx := withCommentConfig(context.Background())

	assert.Equal(t, ";", gitCommentConfig(ctx).prefix)
	testutil.RunGit(t, dir, "config", "core.commentChar", "%")
	assert.Equal(t, ";", gitCommentConfig(ctx).prefix, "the hook keeps its first read")
	assert.Equal(t, "%", gitCommentConfig(context.Background()).prefix, "outside a hook every call reads")
}

// A read that fails yields git's defaults, not the config, so the hook must
// read again rather than keep them.
func TestGitCommentConfig_HookRetriesAFailedRead(t *testing.T) {
	t.Run("done context", func(t *testing.T) {
		testutil.IsolateGitConfigEnv(t)
		dir := resolvedTempDir(t)
		testutil.InitRepo(t, dir)
		testutil.RunGit(t, dir, "config", "core.commentChar", ";")
		t.Chdir(dir)
		ctx := withCommentConfig(context.Background())
		cancelled, cancel := context.WithCancel(ctx)
		cancel()

		assert.Equal(t, "#", gitCommentConfig(cancelled).prefix, "precondition: a failed read falls back to git's default")
		assert.Equal(t, ";", gitCommentConfig(ctx).prefix, "the fallback must not be cached")
	})
	t.Run("unreadable config", func(t *testing.T) {
		testutil.IsolateGitConfigEnv(t)
		dir := resolvedTempDir(t)
		testutil.InitRepo(t, dir)
		configPath := filepath.Join(dir, ".git", "config")
		valid, err := os.ReadFile(configPath)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(configPath, append(valid, []byte("[core\ncommentChar = %\n")...), 0o600))
		t.Chdir(dir)
		ctx := withCommentConfig(context.Background())

		assert.Equal(t, "#", gitCommentConfig(ctx).prefix, "precondition: a broken config falls back to git's default")
		require.NoError(t, os.WriteFile(configPath, append(valid, []byte("[core]\ncommentChar = %\n")...), 0o600))
		assert.Equal(t, "%", gitCommentConfig(ctx).prefix, "the fallback must not be cached")
	})
}
