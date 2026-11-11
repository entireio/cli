package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/stretchr/testify/require"
)

func TestProjectAgentExecuteStages(t *testing.T) {
	// The executor uses the per-user cache root; isolate it for this test.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for _, stage := range []string{"success", "clone", "checkout", "diff", "claude"} {
		t.Run(stage, func(t *testing.T) {
			var calls []string
			e := projectAgentExecutor{git: "/trusted/git"}
			e.run = func(_ context.Context, binary, _ string, _, args []string, _ string) ([]byte, error) {
				kind := "claude"
				for _, s := range []string{"clone", "checkout", "diff"} {
					for _, arg := range args {
						if arg == s || (s == "diff" && arg == "diff-tree") {
							kind = s
						}
					}
				}
				calls = append(calls, kind)
				if kind == stage {
					return nil, errors.New("mock failure")
				}
				require.Equal(t, e.git, binary)
				require.Contains(t, args, "core.symlinks=false")
				require.Contains(t, args, "protocol.allow=never")
				if kind == "diff" {
					require.Contains(t, args, "--no-ext-diff")
					require.Contains(t, args, "--no-textconv")
				}
				return []byte("diff"), nil
			}
			e.review = func(_ context.Context, cfg claudecode.RestrictedReview) (json.RawMessage, error) {
				calls = append(calls, "claude")
				if stage == "claude" {
					return nil, errors.New("mock failure")
				}
				require.Equal(t, "repo", filepath.Base(cfg.Directory))
				require.Equal(t, "review", filepath.Base(cfg.ReadDir))
				require.Contains(t, cfg.Prompt, "change.diff")
				require.JSONEq(t, string(agentServeClaim().OutputSchema), string(cfg.OutputSchema))
				return json.RawMessage(`{"findings":[]}`), nil
			}
			result, err := e.execute(t.Context(), agentServeClaim())
			if stage == "success" {
				require.NoError(t, err)
				require.JSONEq(t, `{"findings":[]}`, string(result))
				require.Equal(t, []string{"clone", "checkout", "diff", "claude"}, calls)
			} else {
				require.Error(t, err)
				require.Equal(t, stage, calls[len(calls)-1])
				require.Nil(t, result)
			}
		})
	}
}

func TestProjectAgentExecuteCredentialBoundary(t *testing.T) {
	t.Setenv("ENTIRE_TOKEN", "runner-secret")
	t.Setenv("OTHER_SECRET", "other-secret")
	t.Setenv("NODE_OPTIONS", "--require malicious")
	t.Setenv("GIT_DIR", "/bad/repo")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", "/bad/hooks")
	t.Setenv("CLAUDECODE", "inherited-session")
	t.Setenv("ANTHROPIC_API_KEY", "model-key")
	git := strings.Join(projectAgentGitEnv(), "\n")
	require.Contains(t, git, "ENTIRE_TOKEN=runner-secret")
	for _, key := range []string{"GIT_DIR=", "GIT_CONFIG_COUNT=", "GIT_CONFIG_KEY_0=", "GIT_CONFIG_VALUE_0="} {
		require.NotContains(t, git, key)
	}
}

func TestProjectAgentExecuteRefusesUnsafeClaims(t *testing.T) {
	t.Parallel()
	for _, remote := range []string{"file:///etc", "ext::sh evil", "https://example.com/repo", "entire://user:secret@example.com/repo", "entire://example.com/repo?token=secret"} {
		t.Run(remote, func(t *testing.T) {
			t.Parallel()
			claim := agentServeClaim()
			claim.GitRemoteURL = remote
			require.Error(t, validateProjectAgentClaim(claim))
		})
	}
	c := agentServeClaim()
	c.HeadSHA = "--upload-pack=evil"
	require.Error(t, validateProjectAgentClaim(c))
}

func TestProjectAgentLimitedOutput(t *testing.T) {
	t.Parallel()
	var b projectAgentLimitedBuffer
	_, err := b.Write(make([]byte, agentOutputLimit+1))
	require.Error(t, err)
	require.Zero(t, b.Len())
}

func TestProjectAgentProcess(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	root, err := osroot.Shared(dir)
	require.NoError(t, err)
	t.Cleanup(func() { osroot.Forget(dir) })
	path := filepath.Join(dir, "fake-claude")
	require.NoError(t, osroot.WriteFile(root, "fake-claude", []byte("#!/bin/sh\nprintf '%s' '{\"structured_output\":{\"findings\":[]}}'\n"), 0o700))
	result, err := runProjectAgentProcess(t.Context(), path, dir, []string{"PATH=/usr/bin:/bin"}, nil, "")
	require.NoError(t, err)
	require.True(t, json.Valid(result))
	require.NoError(t, osroot.WriteFile(root, "fake-claude", []byte("#!/bin/sh\necho secret >&2\nexit 1\n"), 0o700))
	_, err = runProjectAgentProcess(t.Context(), path, dir, []string{"PATH=/usr/bin:/bin"}, nil, "")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
	require.NoError(t, osroot.WriteFile(root, "fake-claude", []byte("#!/bin/sh\nsleep 30\n"), 0o700))
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = runProjectAgentProcess(ctx, path, dir, []string{"PATH=/usr/bin:/bin"}, nil, "")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestProjectAgentExecuteCancelledBeforeCheckout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	e := projectAgentExecutor{run: func(context.Context, string, string, []string, []string, string) ([]byte, error) {
		t.Fatal("must not execute")
		return nil, nil
	}}
	_, err := e.execute(ctx, agentServeClaim())
	require.ErrorIs(t, err, context.Canceled)
}
