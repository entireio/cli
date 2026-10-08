package cli

import (
	"bytes"
	"errors"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/stretchr/testify/require"
)

func TestDoctorCodexWarningsNamePathOwnershipAndUserRemedies(t *testing.T) {
	t.Parallel()

	t.Run("linked worktree mismatch", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		writeCodexInactiveWorktreeWarning(
			&output,
			"/repo-feature/.codex/hooks.json",
			"/repo-main/.codex/hooks.json",
		)

		out := output.String()
		require.Contains(t, out, "NOT ACTIVE IN THIS WORKTREE")
		require.Contains(t, out, "/repo-feature/.codex/hooks.json")
		require.Contains(t, out, "/repo-main/.codex/hooks.json")
		require.Contains(t, out, "Codex will read the discovered file above, not the current-worktree file above")
		require.Contains(t, out, ".codex/hooks.json is tracked — commit it and make sure the root worktree has it")
		require.Contains(t, out, "(merge to the default branch, or check that branch out there).")
		require.NotContains(t, out, "If that root is a Git checkout")
		require.NotContains(t, out, "In a .bare layout")
		require.NotContains(t, out, "migrate")
		require.NotContains(t, out, "synchronize")
	})

	t.Run("invalid discovered file", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		writeCodexDiscoveredInspectionWarning(
			&output,
			"/repo-main/.codex/hooks.json",
			codex.HookFileMalformed,
			errors.New("Codex hooks file exceeds 1048576 bytes"),
		)

		out := output.String()
		require.Contains(t, out, "/repo-main/.codex/hooks.json")
		require.Contains(t, out, "exceeds 1048576 bytes")
		require.Contains(t, out, "Fix this discovered .codex/hooks.json file in its project root")
		require.Contains(t, out, ".codex/hooks.json is tracked — commit it and make sure the root worktree has it")
		require.NotContains(t, out, "In a .bare layout")
	})

	t.Run("invalid current-worktree file", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		writeCodexWorktreeInspectionWarning(
			&output,
			"/repo-feature/.codex/hooks.json",
			codex.HookFileMalformed,
			errors.New("repository .codex path is a redirected directory"),
		)

		out := output.String()
		require.Contains(t, out, "MALFORMED CURRENT-WORKTREE CONFIGURATION")
		require.Contains(t, out, "/repo-feature/.codex/hooks.json")
		require.Contains(t, out, "redirected directory")
		require.Contains(t, out, "run `entire enable --force`")
		require.Contains(t, out, "This may not be the file Codex reads")
		require.Contains(t, out, "make sure the discovered project root has it too")
		require.NotContains(t, out, "do not run `entire enable` from it")
	})

	t.Run("missing project layer", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		writeCodexMissingProjectLayerWarning(
			&output,
			"/repo-feature/.codex",
			"/repo-main/.codex/hooks.json",
		)

		out := output.String()
		require.Contains(t, out, "/repo-feature/.codex (missing)")
		require.Contains(t, out, "/repo-main/.codex/hooks.json")
		require.Contains(t, out, ".codex/hooks.json is tracked — commit it and make sure the root worktree has it")
		require.NotContains(t, out, "In a .bare layout")
	})
}

func TestCodexStatusWarningBehavior(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		issue *codexHookIssue
		want  string
	}{
		{
			name: "linked worktree mismatch",
			issue: &codexHookIssue{
				State:          codexHookStateInactiveWorktreePath,
				WorktreePath:   "/repo-feature/.codex/hooks.json",
				DiscoveredPath: "/repo-main/.codex/hooks.json",
			},
			want: "not active in this worktree",
		},
		{
			name:  "malformed discovery",
			issue: &codexHookIssue{State: codexHookStateMalformedDiscovered},
			want:  "discovered hooks are malformed",
		},
		{
			name:  "unavailable current worktree",
			issue: &codexHookIssue{State: codexHookStateUnavailableWorktree},
			want:  "Current-worktree Codex hooks are unavailable",
		},
		{
			name:  "project layer",
			issue: &codexHookIssue{State: codexHookStateProjectLayerMissing},
			want:  "project layer missing",
		},
		{
			name:  "trust gaps",
			issue: &codexHookIssue{State: codexHookStateTrustReview, MissingApprovals: []string{"stop", "post_tool_use"}},
			want:  "2 Codex hook(s) need approval · open /hooks",
		},
		{
			name:  "active via root checkout",
			issue: &codexHookIssue{State: codexHookStateWorktreePathNotDiscovered},
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			warning := codexStatusWarning(tt.issue)
			if tt.want == "" {
				require.Empty(t, warning)
				return
			}
			require.Contains(t, warning, tt.want)
		})
	}
}

func TestCodexSessionStartWarningsStayConcise(t *testing.T) {
	t.Parallel()

	mismatch := codexSessionStartWarning(&codexHookIssue{State: codexHookStateInactiveWorktreePath})
	require.Equal(t, "Entire hooks in this worktree are not active; Codex discovers another checkout. Run 'entire doctor'.", mismatch)
	require.NotContains(t, mismatch, "/repo-main")

	malformedWorktree := codexSessionStartWarning(&codexHookIssue{State: codexHookStateMalformedWorktree})
	require.Equal(t, "This worktree's Codex hooks configuration is malformed. Run 'entire doctor'.", malformedWorktree)

	trust := codexSessionStartWarning(&codexHookIssue{
		State:            codexHookStateTrustReview,
		MissingApprovals: []string{"post_tool_use", "subagent_start"},
	})
	require.Equal(t, "2 Codex hook(s) await approval. Open /hooks.", trust)
	require.NotContains(t, trust, "trusted_hash")

	active := codexSessionStartWarning(&codexHookIssue{State: codexHookStateWorktreePathNotDiscovered})
	require.Empty(t, active)
}

func TestCodexHooksStatusJSONPreservesDiagnosticPaths(t *testing.T) {
	t.Parallel()
	status := codexHooksStatusFromIssue(&codexHookIssue{
		State:            codexHookStateInactiveWorktreePath,
		WorktreePath:     "/repo-feature/.codex/hooks.json",
		DiscoveredPath:   "/repo-main/.codex/hooks.json",
		MissingApprovals: []string{"post_tool_use"},
	})

	require.Equal(t, codexHookStateInactiveWorktreePath, status.State)
	require.Equal(t, "/repo-feature/.codex/hooks.json", status.WorktreePath)
	require.Equal(t, "/repo-main/.codex/hooks.json", status.DiscoveredPath)
	require.Equal(t, []string{"post_tool_use"}, status.MissingApprovals)
}
