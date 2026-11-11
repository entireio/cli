package claudecode

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRestrictedReviewContract(t *testing.T) {
	t.Parallel()
	calls := 0
	run := func(ctx context.Context, binary, dir string, env, args []string, input string) ([]byte, error) {
		calls++
		require.Equal(t, "/trusted/claude", binary)
		require.Contains(t, args, flagRestricted)
		settingsIndex := slices.Index(args, flagSettingSources)
		require.NotEqual(t, -1, settingsIndex)
		require.Less(t, settingsIndex+1, len(args))
		require.Empty(t, args[settingsIndex+1], "probe and review must disable all settings sources")
		if calls == 1 {
			require.Contains(t, args, "--help")
			require.Empty(t, input)
			_, ok := ctx.Deadline()
			require.True(t, ok)
			return []byte("--restricted --strict-mcp-config --json-schema --setting-sources"), nil
		}
		require.Equal(t, "/checkout", dir)
		require.Equal(t, "review prompt", input)
		require.Contains(t, args, flagOutputFormat)
		require.Contains(t, args, flagStrictMCP)
		require.Contains(t, args, `{"mcpServers":{}}`)
		require.Contains(t, args, `{"disableAllHooks":true}`)
		require.Contains(t, args, "--no-session-persistence")
		require.Contains(t, args, "Read,Grep,Glob")
		require.Contains(t, args, "/diff")
		require.Contains(t, args, "sonnet")
		require.Contains(t, args, `{"type":"object"}`)
		require.NotContains(t, args, "--dangerously-skip-permissions")
		for _, entry := range env {
			require.False(t, strings.HasPrefix(entry, "ENTIRE_"))
		}
		return []byte(`{"type":"result","subtype":"success","structured_output":{"findings":[]}}`), nil
	}
	reviewer, err := prepareRestrictedReviewer(t.Context(), "/trusted/claude", run)
	require.NoError(t, err)
	result, err := reviewer.Review(t.Context(), RestrictedReview{Directory: "/checkout", ReadDir: "/diff", Prompt: "review prompt", Model: "sonnet", OutputSchema: json.RawMessage(`{"type":"object"}`)})
	require.NoError(t, err)
	require.JSONEq(t, `{"findings":[]}`, string(result))
	require.Equal(t, 2, calls)
}

func TestRestrictedReviewNoFallback(t *testing.T) {
	t.Parallel()
	for _, output := range []string{"--help", "--restricted", "--restricted --strict-mcp-config", "--restricted --strict-mcp-config --json-schema"} {
		t.Run(output, func(t *testing.T) {
			t.Parallel()
			calls := 0
			_, err := prepareRestrictedReviewer(t.Context(), "/trusted/claude", func(context.Context, string, string, []string, []string, string) ([]byte, error) {
				calls++
				return []byte(output), nil
			})
			require.ErrorContains(t, err, "no unrestricted fallback")
			require.Equal(t, 1, calls)
		})
	}
	_, err := prepareRestrictedReviewer(t.Context(), "/trusted/claude", func(context.Context, string, string, []string, []string, string) ([]byte, error) {
		return nil, context.Canceled
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestRestrictedReviewCredentialBoundary(t *testing.T) {
	t.Setenv("ENTIRE_TOKEN", "runner-secret")
	t.Setenv("OTHER_SECRET", "other-secret")
	t.Setenv("NODE_OPTIONS", "--require malicious")
	t.Setenv("GIT_DIR", "/bad/repo")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("CLAUDECODE", "inherited-session")
	t.Setenv("ANTHROPIC_API_KEY", "model-key")
	t.Setenv("USER", "operator")
	t.Setenv("HTTPS_PROXY", "http://proxy.internal:3128")
	t.Setenv("NODE_EXTRA_CA_CERTS", "/etc/corp-ca.pem")
	entries, err := restrictedReviewEnv()
	require.NoError(t, err)
	env := strings.Join(entries, "\n")
	for _, want := range []string{"ANTHROPIC_API_KEY=model-key", "USER=operator", "HTTPS_PROXY=http://proxy.internal:3128", "NODE_EXTRA_CA_CERTS=/etc/corp-ca.pem"} {
		require.Contains(t, env, want)
	}
	for _, secret := range []string{"runner-secret", "other-secret", "NODE_OPTIONS=", "GIT_DIR=", "GIT_CONFIG_COUNT=", "CLAUDECODE="} {
		require.NotContains(t, env, secret)
	}
}

func TestRestrictedReviewResponse(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{`not json`, `{}`, `{"type":"result","subtype":"success","structured_output":null}`, `{"type":"result","subtype":"error_max_turns","structured_output":{}}`} {
		_, err := parseRestrictedReviewResponse(t.Context(), []byte(bad))
		require.Error(t, err)
	}
	out, err := parseRestrictedReviewResponse(t.Context(), []byte(`{"type":"result","subtype":"success","permission_denials":[{}],"structured_output":{"findings":[]}}`))
	require.NoError(t, err, "a denied tool call must not discard the review")
	require.JSONEq(t, `{"findings":[]}`, string(out))
	_, err = parseRestrictedReviewResponse(t.Context(), []byte(`{"type":"result","is_error":true,"api_error_status":401,"result":"secret review content","structured_output":{}}`))
	var classified *ClaudeError
	require.ErrorAs(t, err, &classified)
	require.NotContains(t, err.Error(), "secret review content")
}
