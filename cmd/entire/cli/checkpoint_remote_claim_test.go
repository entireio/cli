package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	checkpointremote "github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
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
		// The repo field is read from the COMMITTED settings.json — the
		// inherited-from-upstream case this feature is about — and the output
		// is a command a human is told to run. A hostile repository must not be
		// able to put anything executable in it.
		{"shell separator", settings.CheckpointRemoteConfig{Provider: "github", Repo: "acme/foo;id"}, ""},
		{"backticks", settings.CheckpointRemoteConfig{Provider: "github", Repo: "acme/foo`id`"}, ""},
		{"command substitution", settings.CheckpointRemoteConfig{Provider: "github", Repo: "acme/foo$(id)"}, ""},
		{"pipe", settings.CheckpointRemoteConfig{Provider: "github", Repo: "acme/foo|id"}, ""},
		{"and-and", settings.CheckpointRemoteConfig{Provider: "github", Repo: "acme/foo&&id"}, ""},
		{"newline", settings.CheckpointRemoteConfig{Provider: "github", Repo: "acme/foo\nid"}, ""},
		{"quote", settings.CheckpointRemoteConfig{Provider: "github", Repo: "acme/fo\"o"}, ""},
		{"redirect", settings.CheckpointRemoteConfig{Provider: "github", Repo: "acme/foo>out"}, ""},
		{"three segments", settings.CheckpointRemoteConfig{Provider: "github", Repo: "acme/foo/bar"}, ""},
		// Surrounding whitespace is tolerated because the validation trims —
		// but then the value WRITTEN must be the trimmed one too. Building the
		// command and the write separately let this pass the check and fail the
		// write, prompting the user and then erroring.
		{"padded fields", settings.CheckpointRemoteConfig{Provider: " github ", Repo: " acme/checkpoints "}, "github:acme/checkpoints"},
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

			// The value written on the user's behalf must be the same one the
			// printed command carries, or the prompt accepts a claim the write
			// then rejects.
			assert.Equal(t, tc.wantFlag, checkpointremote.ClaimCheckpointRemoteFlagValue(&tc.config),
				"the written value must match the printed command")

			provider, repo, err := parseCheckpointRemoteFlag(tc.wantFlag)
			require.NoError(t, err, "the offered command must parse")
			// Trimmed, because that is what the value carries and what gets
			// written — the padded case exists to pin exactly that.
			assert.Equal(t, strings.TrimSpace(tc.config.Provider), provider)
			assert.Equal(t, strings.TrimSpace(tc.config.Repo), repo)
		})
	}

	assert.Empty(t, checkpointremote.ClaimCheckpointRemoteCommand(nil))
}

// TestEnableReportsAnIgnoredCheckpointRemote fills the enable gap: `entire
// enable` is where checkpoint configuration is fixed, and a checkpoint_remote
// the ownership check refused used to leave it saying nothing at all — the
// rejection reached the user only through .entire/logs.
func TestEnableReportsAnIgnoredCheckpointRemote(t *testing.T) {
	dir := setupTestRepo(t)
	// origin belongs to alice, the configured store to acme: inherited by
	// cloning as far as local git config can tell.
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/alice/app.git")
	writeSettings(t, `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`)

	ctx := context.Background()
	s, err := settings.Load(ctx)
	require.NoError(t, err)

	var out bytes.Buffer
	reportIgnoredCheckpointRemote(ctx, &out, s, "origin")

	got := out.String()
	assert.Contains(t, got, "acme/checkpoints is not in use")
	assert.Contains(t, got, "entire enable --local --checkpoint-remote github:acme/checkpoints")
}

// TestEnableSaysNothingAboutACheckpointRemoteInUse is the control: the report
// is above the `touched` gate, so it prints on every `entire enable`, and a
// false positive would tell a correctly configured repo to fix itself.
func TestEnableSaysNothingAboutACheckpointRemoteInUse(t *testing.T) {
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/acme/app.git")
	writeSettings(t, `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`)

	ctx := context.Background()
	s, err := settings.Load(ctx)
	require.NoError(t, err)

	var out bytes.Buffer
	reportIgnoredCheckpointRemote(ctx, &out, s, "origin")
	assert.Empty(t, out.String(), "a store whose owner matches every remote is the developer's own")
}

// TestEnableCommandSurfacesAnIgnoredCheckpointRemote proves the report is
// reached by `entire enable` itself, and not just callable. The report sits
// above the flow's `touched` gate precisely so a bare re-enable — the
// invocation someone reaches for when checkpoints are not where they expected —
// still names the fix.
func TestEnableCommandSurfacesAnIgnoredCheckpointRemote(t *testing.T) {
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/alice/app.git")
	writeSettings(t, `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`)

	cmd := newEnableCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())

	assert.Contains(t, output.String(), "checkpoint_remote acme/checkpoints is not in use")
	assert.Contains(t, output.String(), "entire enable --local --checkpoint-remote github:acme/checkpoints")
}

// TestEnableDoesNotOfferAStoreAnotherOwnerHolds is the guard on the offer: it
// exists for ownership that is UNPROVABLE, never for ownership that is
// DISPROVED. Origin here names a different owner, which is the fork case the
// check was built for — offering to adopt would walk a contributor into
// publishing their transcripts to the upstream's store one keystroke deep.
//
// Asserted through the verdict rather than the prompt, because the prompt is
// suppressed under test anyway and the verdict is what gates it.
func TestEnableDoesNotOfferAStoreAnotherOwnerHolds(t *testing.T) {
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/alice/app.git")
	writeSettings(t, `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`)

	ctx := context.Background()
	s, err := settings.Load(ctx)
	require.NoError(t, err)

	verdict, reason := checkpointremote.InheritedCheckpointRemoteVerdict(ctx, s, "origin")
	require.True(t, verdict.Refused(), "a differently-owned store must be refused")
	assert.Equal(t, checkpointremote.OwnershipDisproved, verdict,
		"a readable, mismatched owner is disproof, not absence of evidence: %s", reason)
}

// TestEnableOffersOnlyWhenOwnershipCannotBeEstablished is the other side: a
// single-segment origin yields no owner to compare, which is ordinary on
// self-hosted git and is the case a human can settle.
func TestEnableOffersOnlyWhenOwnershipCannotBeEstablished(t *testing.T) {
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "git@selfhosted.example:app.git")
	writeSettings(t, `{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"acme/checkpoints"}}}`)

	ctx := context.Background()
	s, err := settings.Load(ctx)
	require.NoError(t, err)

	verdict, _ := checkpointremote.InheritedCheckpointRemoteVerdict(ctx, s, "origin")
	require.True(t, verdict.Refused(), "an unprovable store is still refused non-interactively")
	assert.Equal(t, checkpointremote.OwnershipUnprovable, verdict)
}
