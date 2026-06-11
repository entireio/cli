package investigate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestSaveInvestigateConfig_WritesLocalFile verifies that
// saveInvestigateConfig persists into .entire/settings.local.json (not the
// committed .entire/settings.json). Mirrors the review-side behaviour so
// agent picker output stays out of project settings.
//
// NOTE: This test uses t.Chdir, which Go forbids combining with
// t.Parallel(). Do not add t.Parallel() here.
func TestSaveInvestigateConfig_WritesLocalFile(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	testutil.InitRepo(t, tmp)

	cfg := &settings.InvestigateConfig{
		Agents:   []string{"claude-code", "codex"},
		MaxTurns: 4,
		Quorum:   2,
	}
	require.NoError(t, saveInvestigateConfig(context.Background(), cfg))

	// settings.json should NOT contain investigate.
	base, err := os.ReadFile(filepath.Join(tmp, ".entire/settings.json"))
	if err == nil {
		require.NotContains(t, string(base), `"investigate"`,
			"investigate must not be written to project settings")
	}

	// settings.local.json should contain investigate.
	local, err := os.ReadFile(filepath.Join(tmp, ".entire/settings.local.json"))
	require.NoError(t, err)
	require.Contains(t, string(local), `"agents"`)
	require.Contains(t, string(local), `"claude-code"`)
}

// TestResolveDocPaths_PerRunIsolation verifies that two runs land in
// distinct per-run directories under the git common dir, so they don't
// stomp each other's findings/state files.
func TestResolveDocPaths_PerRunIsolation(t *testing.T) {
	t.Parallel()

	const commonDir = "/repo/.git"

	findings1 := resolveDocPaths(commonDir, "aaaaaaaaaaaa")
	findings2 := resolveDocPaths(commonDir, "bbbbbbbbbbbb")

	require.Equal(t,
		filepath.Join(commonDir, "entire-investigations", "aaaaaaaaaaaa", "findings.md"),
		findings1,
	)
	require.Equal(t,
		filepath.Join(commonDir, "entire-investigations", "bbbbbbbbbbbb", "findings.md"),
		findings2,
	)
	require.NotEqual(t, findings1, findings2,
		"two runs must not share findings doc paths")
}

// TestConfirmUntrustedIssueSeed_DeclinedExitsCleanly verifies that when
// the operator declines the "issue-link arms an externally-seeded
// investigation" confirmation, the function returns ok=false so runFresh
// exits without launching agents.
func TestConfirmUntrustedIssueSeed_DeclinedExitsCleanly(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(&stderr)

	deps := Deps{
		PromptYN: func(_ context.Context, _ string, _ bool) (bool, error) {
			return false, nil
		},
	}
	ok, err := confirmUntrustedIssueSeed(context.Background(), cmd, deps, "https://github.com/o/r/issues/1", false)
	require.NoError(t, err)
	require.False(t, ok, "decline must surface as ok=false")
	require.Contains(t, stderr.String(), "permission/sandbox bypass",
		"warning must explain the bypass risk so the operator can make an informed call")
}

// TestConfirmUntrustedIssueSeed_AcceptedReturnsOK verifies the happy path.
func TestConfirmUntrustedIssueSeed_AcceptedReturnsOK(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetOut(&bytes.Buffer{})

	deps := Deps{
		PromptYN: func(_ context.Context, _ string, _ bool) (bool, error) {
			return true, nil
		},
	}
	ok, err := confirmUntrustedIssueSeed(context.Background(), cmd, deps, "https://github.com/o/r/issues/1", false)
	require.NoError(t, err)
	require.True(t, ok)
}

// TestConfirmUntrustedIssueSeed_PromptError surfaces prompt-transport
// failures so runFresh can bail with a wrapped error instead of running
// agents blind.
func TestConfirmUntrustedIssueSeed_PromptError(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetOut(&bytes.Buffer{})

	wantErr := errors.New("simulated prompt failure")
	deps := Deps{
		PromptYN: func(_ context.Context, _ string, _ bool) (bool, error) {
			return false, wantErr
		},
	}
	ok, err := confirmUntrustedIssueSeed(context.Background(), cmd, deps, "https://github.com/o/r/issues/1", false)
	require.False(t, ok)
	require.ErrorIs(t, err, wantErr)
}

// TestConfirmUntrustedIssueSeed_NonInteractiveRefusesWithoutOptIn verifies the
// strict default: with no TTY to prompt and --allow-untrusted-seed unset, the
// run is refused rather than silently proceeding with attacker-influenced
// content into a bypass-mode agent. Deps with a nil PromptYN drives the
// non-interactive branch (CanPromptInteractively is false under test).
func TestConfirmUntrustedIssueSeed_NonInteractiveRefusesWithoutOptIn(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(&stderr)

	ok, err := confirmUntrustedIssueSeed(context.Background(), cmd, Deps{}, "https://github.com/o/r/issues/1", false)
	require.False(t, ok, "must refuse non-interactively without opt-in")
	require.ErrorIs(t, err, errUntrustedSeedRefused)
	require.Contains(t, stderr.String(), "--allow-untrusted-seed",
		"refusal message must name the opt-in flag")
}

// TestConfirmUntrustedIssueSeed_NonInteractiveProceedsWithOptIn verifies that
// the explicit opt-in restores automation: --allow-untrusted-seed proceeds
// non-interactively, with the risk still logged to stderr.
func TestConfirmUntrustedIssueSeed_NonInteractiveProceedsWithOptIn(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(&stderr)

	ok, err := confirmUntrustedIssueSeed(context.Background(), cmd, Deps{}, "https://github.com/o/r/issues/1", true)
	require.NoError(t, err)
	require.True(t, ok, "must proceed non-interactively when opted in")
	require.Contains(t, stderr.String(), "permission/sandbox bypass",
		"warning must still surface the risk even when proceeding")
}
