package strategy

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// newGloballyEnrolledPromptRepo creates a globally enrolled repo (no
// repo-level setup) and chdirs into it. Not parallel-safe: t.Chdir + t.Setenv.
func newGloballyEnrolledPromptRepo(t *testing.T, userSettingsJSON string) {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.AddRemote(t, dir, "origin", "https://github.com/acme/widgets.git")
	t.Chdir(dir)
	enrollRepoGlobally(t, userSettingsJSON)
}

// TestResolveTrustDecision: a non-interactive pre-push holds WITHOUT
// prompting (stdin carries git ref lines); answers persist through the same
// writers `entire trust` uses; a failed prompt or trust write holds.
func TestResolveTrustDecision(t *testing.T) {
	askErr := errors.New("trust prompt: terminal gone")
	for _, tc := range []struct {
		name           string
		unconfigured   bool // no global block: the trust write fails
		nonInteractive bool
		choice         trustChoice
		askErr         error
		wantErr        error
		want           TrustDecision
		wantSource     settings.TrustSource
		wantWarn       bool
	}{
		{name: "non-interactive holds without prompting", nonInteractive: true, want: TrustHeld},
		{name: "yes persists repo trust", choice: trustChoiceYes, want: TrustGranted, wantSource: settings.TrustSourceRepo},
		{name: "always sets trust_all", choice: trustChoiceAlways, want: TrustGranted, wantSource: settings.TrustSourceAll},
		{name: "not now holds and writes nothing", choice: trustChoiceNotNow, want: TrustHeld},
		{name: "ask error holds and propagates", choice: trustChoiceYes, askErr: askErr, wantErr: askErr, want: TrustHeld},
		{name: "trust write failure warns and holds", unconfigured: true, choice: trustChoiceYes, want: TrustHeld, wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userSettings := `{"global":{"enabled":true}}`
			if tc.unconfigured {
				userSettings = `{}`
			}
			newGloballyEnrolledPromptRepo(t, userSettings)
			ctx := context.Background()
			ask := func() (trustChoice, error) {
				if tc.nonInteractive {
					t.Fatal("prompt must not open in a non-interactive pre-push")
				}
				return tc.choice, tc.askErr
			}

			var warnings bytes.Buffer
			d, err := resolveTrustDecision(ctx, !tc.nonInteractive, ask, &warnings)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.want, d)
			if tc.wantSource == "" {
				tc.wantSource = settings.TrustSourceNone
			}
			require.Equal(t, tc.wantSource, settings.CurrentTrustSource(ctx))
			if tc.wantWarn {
				require.Contains(t, warnings.String(), "Warning")
			}
		})
	}
}

func TestTrustPromptDefaultHolds(t *testing.T) {
	t.Parallel()

	require.Equal(t, trustChoiceNotNow, trustChoiceDefault)
	var warnings bytes.Buffer
	require.Equal(t, TrustHeld, applyTrustChoice(t.Context(), trustChoiceDefault, &warnings))
	require.Empty(t, warnings.String())
}
