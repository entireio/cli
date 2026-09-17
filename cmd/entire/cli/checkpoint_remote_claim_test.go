package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	checkpointremote "github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// TestClaimCommandParsesAsACheckpointRemoteFlag is the pin between the remedy
// and the flag it tells the user to run. ClaimCheckpointRemoteCommand lives in
// the remote package, which cannot import the flag parser without a cycle, so
// it re-derives what a valid provider and repo look like; this test is what
// stops the two drifting into a remedy that errors when pasted.
func TestClaimCommandParsesAsACheckpointRemoteFlag(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		config   settings.CheckpointRemoteConfig
		wantFlag string // "" means no command should be offered at all
	}{
		{"ordinary", settings.CheckpointRemoteConfig{Provider: "github", Repo: "acme/checkpoints"}, "github:acme/checkpoints"},
		{"dotted repo name", settings.CheckpointRemoteConfig{Provider: "github", Repo: "acme/checkpoints.store"}, "github:acme/checkpoints.store"},
		{"no owner", settings.CheckpointRemoteConfig{Provider: "github", Repo: "checkpoints"}, ""},
		{"empty repo", settings.CheckpointRemoteConfig{Provider: "github", Repo: ""}, ""},
		{"empty provider", settings.CheckpointRemoteConfig{Provider: "", Repo: "acme/checkpoints"}, ""},
		{"provider carrying the separator", settings.CheckpointRemoteConfig{Provider: "git:hub", Repo: "acme/checkpoints"}, ""},
		// The resolver maps gitlab (providerHost) but the flag rejects it, so a
		// command naming it would fail when pasted. Offer none until the flag
		// is widened.
		{"provider the flag rejects", settings.CheckpointRemoteConfig{Provider: "gitlab", Repo: "acme/checkpoints"}, ""},
		{"repo with a space", settings.CheckpointRemoteConfig{Provider: "github", Repo: "acme/check points"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := checkpointremote.ClaimCheckpointRemoteCommand(&tc.config)
			if tc.wantFlag == "" {
				assert.Empty(t, got, "a config the flag would reject must offer no command")
				return
			}
			require.Equal(t, "entire enable --local --checkpoint-remote "+tc.wantFlag, got)

			provider, repo, err := parseCheckpointRemoteFlag(tc.wantFlag)
			require.NoError(t, err, "the offered command must parse")
			assert.Equal(t, tc.config.Provider, provider)
			assert.Equal(t, tc.config.Repo, repo)
		})
	}

	assert.Empty(t, checkpointremote.ClaimCheckpointRemoteCommand(nil))
}
