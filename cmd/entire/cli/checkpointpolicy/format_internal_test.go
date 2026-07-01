package checkpointpolicy

import (
	"testing"

	semver "github.com/Masterminds/semver/v3"
	"github.com/stretchr/testify/require"
)

func TestResolveCheckpointVersionSelectorWithCandidates(t *testing.T) {
	t.Parallel()
	candidates := []*semver.Version{
		mustSemver("1.0.0"),
		mustSemver("1.0.4"),
		mustSemver("1.1.2"),
		mustSemver("2.0.0"),
	}

	tests := []struct {
		name     string
		selector string
		want     string
	}{
		{name: "major selects newest matching major", selector: "1", want: "1.1.2"},
		{name: "major minor selects newest patch", selector: "1.0", want: "1.0.4"},
		{name: "full version selects exact patch", selector: "1.0.0", want: "1.0.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveCheckpointVersionSelector(tt.selector, candidates)
			require.NoError(t, err)
			require.Equal(t, tt.want, got.String())
		})
	}
}

func TestResolveCheckpointVersionSelectorReportsUnsupportedSelector(t *testing.T) {
	t.Parallel()

	_, err := resolveCheckpointVersionSelector("3", []*semver.Version{mustSemver("1.0.0")})
	require.ErrorContains(t, err, `checkpoint_version "3" is not writable by this Entire CLI`)
}
