package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestEnableCheckpointPushRemote_ExplicitLocalOnReenable(t *testing.T) {
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/repo.git")
	testutil.RunGit(t, dir, "remote", "add", "fork", "https://github.com/me/repo.git")
	writeSettings(t, `{"enabled":true}`)
	before, err := os.ReadFile(EntireSettingsFile)
	require.NoError(t, err)
	cmd := newEnableCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--checkpoint-push-remote", "fork", "--project"})
	require.NoError(t, cmd.Execute())
	s, err := settings.Load(t.Context())
	require.NoError(t, err)
	require.Equal(t, "fork", s.GetCheckpointPushRemote())
	after, err := os.ReadFile(EntireSettingsFile)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.FileExists(t, filepath.Join(dir, EntireSettingsLocalFile))
	require.Contains(t, output.String(), "Checkpoint destination set to: fork")
	require.Contains(t, output.String(), "Saved to .entire/settings.local.json for this clone.")
	require.Contains(t, output.String(), "Checkpoints will be uploaded when you push to fork.")
}

func TestEnableCheckpointPushRemote_Picker(t *testing.T) {
	for _, tc := range []struct {
		name       string
		config     string
		opts       EnableOptions
		selected   string
		wantPrompt bool
		wantRemote string
	}{
		{name: "choose", selected: "fork", wantPrompt: true, wantRemote: "fork"},
		{name: "keep", wantPrompt: true},
		{name: "existing explicit", config: `{"strategy_options":{"checkpoint_push_remote":"fork"}}`, wantRemote: "fork"},
		{name: "repair", config: `{"strategy_options":{"checkpoint_push_remote":"missing"}}`, selected: "fork", wantPrompt: true, wantRemote: "fork"},
		{name: "preserve invalid", config: `{"strategy_options":{"checkpoint_push_remote":"missing"}}`, wantPrompt: true, wantRemote: "missing"},
		{name: "disabled", config: `{"strategy_options":{"push_sessions":false}}`},
		{name: "pending disabled", opts: EnableOptions{SkipPushSessions: true}},
		{name: "pending dedicated", opts: EnableOptions{CheckpointRemote: "github:org/checkpoints"}},
		{name: "yes", opts: EnableOptions{Yes: true}},
		{name: "dedicated", config: `{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"org/checkpoints"}}}`},
		{name: "invalid explicit with dedicated", config: `{"strategy_options":{"checkpoint_push_remote":"missing","checkpoint_remote":{"provider":"github","repo":"org/checkpoints"}}}`, wantRemote: "missing"},
		{name: "rejected dedicated", config: `{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"stranger/checkpoints"}}}`, selected: "fork", wantPrompt: true, wantRemote: "fork"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := setupTestRepo(t)
			testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/repo.git")
			testutil.RunGit(t, dir, "remote", "add", "fork", "https://secret:password@github.com/me/repo.git")
			if tc.config != "" {
				writeSettings(t, tc.config)
			}
			called := false
			choice, err := prepareEnableCheckpointRemoteSelection(t.Context(), tc.opts, false, func(_ context.Context, options []huh.Option[string]) (string, error) {
				called = true
				require.Empty(t, options[0].Value)
				if tc.name == "keep" {
					require.Contains(t, options[0].Key, "https://github.com/org/repo.git")
				}
				for _, option := range options {
					require.NotContains(t, option.Key, "password")
					require.NotContains(t, option.Key, "secret")
				}
				if strings.Contains(tc.name, "invalid") || tc.name == "repair" {
					require.Contains(t, options[0].Key, "missing")
				}
				return tc.selected, nil
			})
			require.NoError(t, err)
			require.Equal(t, tc.wantPrompt, called)
			require.NoFileExists(t, EntireSettingsLocalFile)
			require.NoError(t, choice.persist(t.Context()))
			s, err := settings.Load(t.Context())
			require.NoError(t, err)
			require.Equal(t, tc.wantRemote, s.GetCheckpointPushRemote())
			if tc.selected == "" {
				require.NoFileExists(t, EntireSettingsLocalFile)
			}
		})
	}
}

func TestEnableCheckpointPushRemote_RepairSoleRemainingRemote(t *testing.T) {
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "fork", "https://github.com/me/repo.git")
	writeSettings(t, `{"strategy_options":{"checkpoint_push_remote":"missing"}}`)
	called := false
	choice, err := prepareEnableCheckpointRemoteSelection(t.Context(), EnableOptions{}, false, func(_ context.Context, options []huh.Option[string]) (string, error) {
		called = true
		require.Len(t, options, 2)
		return "fork", nil
	})
	require.NoError(t, err)
	require.True(t, called)
	require.NoError(t, choice.persist(t.Context()))
}

func TestEnableCheckpointPushRemote_PickerCancellation(t *testing.T) {
	// esc/ctrl+c at the picker means the same as its own "Keep current
	// destination" option; only the command's context going away aborts.
	for _, tc := range []struct {
		cancelErr error
		wantAbort bool
	}{
		{cancelErr: huh.ErrUserAborted},
		{cancelErr: context.Canceled, wantAbort: true},
	} {
		t.Run(tc.cancelErr.Error(), func(t *testing.T) {
			dir := setupTestRepo(t)
			testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/repo.git")
			testutil.RunGit(t, dir, "remote", "add", "fork", "https://github.com/me/repo.git")
			choice, err := prepareEnableCheckpointRemoteSelection(t.Context(), EnableOptions{}, false, func(context.Context, []huh.Option[string]) (string, error) { return "", tc.cancelErr })
			if tc.wantAbort {
				require.ErrorIs(t, err, tc.cancelErr)
			} else {
				require.NoError(t, err)
				require.Empty(t, choice.name)
				require.True(t, choice.offered)
				require.NoError(t, choice.persist(t.Context()))
			}
			require.NoDirExists(t, filepath.Join(dir, ".entire"))
		})
	}
}

// A rejected local settings layer makes the write the picker leads to fail, so
// the question must not be asked at all — answering it would otherwise abort
// `entire enable` and, on fresh setup, discard the agent selection.
func TestEnableCheckpointPushRemote_RejectedLocalLayerSkipsPicker(t *testing.T) {
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/repo.git")
	testutil.RunGit(t, dir, "remote", "add", "fork", "https://github.com/me/repo.git")
	require.NoError(t, os.MkdirAll(filepath.Dir(EntireSettingsLocalFile), 0o755))
	require.NoError(t, os.WriteFile(EntireSettingsLocalFile, []byte(`{"enabled":true}`), 0o644))
	testutil.RunGit(t, dir, "add", "-f", EntireSettingsLocalFile)
	testutil.RunGit(t, dir, "commit", "-m", "track local settings", "--no-gpg-sign")

	s, err := settings.Load(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, s.LocalLayerRejection(), "fixture must produce a rejected local layer")

	choice, err := prepareEnableCheckpointRemoteSelection(t.Context(), EnableOptions{}, false, func(context.Context, []huh.Option[string]) (string, error) {
		t.Fatal("picker offered in a repo where the choice cannot be saved")
		return "", nil
	})
	require.NoError(t, err)
	require.Empty(t, choice.name)
	require.False(t, choice.offered)
}

// The ambiguity note is the only description of multi-remote routing on the
// paths where no picker can run, so it is suppressed only once the user has
// actually been asked.
func TestEnableCheckpointPushRemote_AmbiguityNoteFollowsThePicker(t *testing.T) {
	for _, tc := range []struct {
		name     string
		choice   *enableCheckpointRemoteChoice
		wantNote bool
	}{
		{name: "no prepare step", wantNote: true},
		{name: "prepared but never offered", choice: &enableCheckpointRemoteChoice{}, wantNote: true},
		{name: "offered", choice: &enableCheckpointRemoteChoice{offered: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := setupTestRepo(t)
			testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/repo.git")
			testutil.RunGit(t, dir, "remote", "add", "fork", "https://github.com/me/repo.git")
			ctx := t.Context()
			if tc.choice != nil {
				ctx = context.WithValue(ctx, enableCheckpointRemoteKey{}, tc.choice)
			}
			var out bytes.Buffer
			printSetupCheckpointDestinationNote(ctx, &out)
			if tc.wantNote {
				require.Contains(t, out.String(), "This repo has 2 remotes (fork, origin)")
			} else {
				require.Empty(t, out.String())
			}
		})
	}
}

// The picker must not open for an invocation the command was always going to
// reject: a bad --checkpoint-backend fails before anything asks a question.
func TestEnableCheckpointPushRemote_PickerRunsAfterFlagValidation(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "1")
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/repo.git")
	testutil.RunGit(t, dir, "remote", "add", "fork", "https://github.com/me/repo.git")
	writeSettings(t, `{"enabled":true}`)
	cmd := newEnableCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--checkpoint-backend", "bogus"})
	err := cmd.Execute()
	require.ErrorContains(t, err, "invalid --checkpoint-backend")
	require.NotContains(t, err.Error(), "checkpoint destination selection")
}

func TestEnableCheckpointPushRemote_InvalidBeforeWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		config string
		want   string
	}{
		{"unknown", []string{"missing"}, "", "configured Git remote"},
		{"empty", []string{""}, "", "must not be empty"},
		{"url", []string{"https://github.com/me/repo.git"}, "", "configured Git remote"},
		{"conflict", []string{"fork", "--checkpoint-remote", "github:org/checkpoints"}, "", "cannot be combined"},
		{"empty conflict", []string{"fork", "--checkpoint-remote="}, "", "cannot be combined"},
		{"dedicated", []string{"fork"}, `{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"me/checkpoints"}}}`, "dedicated"},
		{"bad settings", []string{"fork"}, "{", "settings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := setupTestRepo(t)
			testutil.RunGit(t, dir, "remote", "add", "fork", "https://github.com/me/repo.git")
			if tc.config != "" {
				writeSettings(t, tc.config)
			}
			before := testutil.RunGit(t, dir, "status", "--porcelain", "--untracked-files=all")
			config, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
			require.NoError(t, err)
			cmd := newEnableCmd()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(append([]string{"--checkpoint-push-remote"}, tc.args...))
			err = cmd.Execute()
			require.ErrorContains(t, err, tc.want)
			require.Equal(t, before, testutil.RunGit(t, dir, "status", "--porcelain", "--untracked-files=all"))
			after, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
			require.NoError(t, err)
			require.Equal(t, config, after)
			require.NoFileExists(t, filepath.Join(dir, ".git", "hooks", "pre-push"))
		})
	}
}

func TestEnableCheckpointPushRemote_SkipsPicker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		remotes int
	}{
		{"no remotes", 0}, {"one remote", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := setupTestRepo(t)
			for i := range tc.remotes {
				testutil.RunGit(t, dir, "remote", "add", []string{"origin", "fork"}[i], "https://github.com/org/repo.git")
			}
			choice, err := prepareEnableCheckpointRemoteSelection(t.Context(), EnableOptions{}, false, func(context.Context, []huh.Option[string]) (string, error) {
				t.Fatal("unexpected picker")
				return "", nil
			})
			require.NoError(t, err)
			require.NoError(t, choice.persist(t.Context()))
			require.NoDirExists(t, filepath.Join(dir, ".entire"))
		})
	}
}

func TestEnableCheckpointPushRemote_ExplicitDisabledAndIdempotent(t *testing.T) {
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/repo.git")
	writeSettings(t, `{"enabled":true,"strategy_options":{"push_sessions":false}}`)
	for i := range 2 {
		cmd := newEnableCmd()
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs([]string{"--checkpoint-push-remote", "origin"})
		require.NoError(t, cmd.Execute())
		require.Contains(t, output.String(), "Checkpoint pushing remains disabled.")
		require.NotContains(t, output.String(), "will be uploaded")
		if i == 0 {
			require.Contains(t, output.String(), "Checkpoint destination set to: origin")
		} else {
			require.Contains(t, output.String(), "No checkpoint destination settings changed.")
		}
		s, err := settings.Load(t.Context())
		require.NoError(t, err)
		require.True(t, s.IsPushSessionsDisabled())
		require.Equal(t, "origin", s.GetCheckpointPushRemote())
	}
}

func TestEnableCheckpointPushRemote_ExplicitSameLocalDoesNotWrite(t *testing.T) {
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "fork", "https://github.com/me/repo.git")
	writeSettings(t, `{"enabled":true}`)
	const local = "{\n  \"strategy_options\": {\"checkpoint_push_remote\": \"fork\", \"push_sessions\": false}\n}\n"
	testutil.WriteFile(t, dir, EntireSettingsLocalFile, local)
	choice, err := prepareEnableCheckpointRemoteSelection(t.Context(), EnableOptions{CheckpointPushRemote: "fork", Yes: true}, true, nil)
	require.NoError(t, err)
	require.NoError(t, choice.persist(t.Context()))
	require.False(t, choice.changed)
	after, err := os.ReadFile(EntireSettingsLocalFile)
	require.NoError(t, err)
	require.Equal(t, []byte(local), after) //nolint:testifylint // Idempotence must preserve bytes and formatting, not merely equivalent JSON.
}

// A healthy destination nobody asked about is not reported, so an ordinary
// enable ends at "Ready."; a broken one is reported on every run; and "No
// checkpoint destination settings changed." appears only when a change was
// asked for and turned out to be a no-op.
func TestEnableCheckpointPushRemote_Report(t *testing.T) {
	const dedicatedURL = "https://github.com/org/checkpoints.git"
	for _, tc := range []struct {
		name, config, want string
		choice             enableCheckpointRemoteChoice
		wantUnchangedLine  bool
	}{
		{name: "ordinary untouched", config: `{"enabled":true}`},
		{name: "ordinary offered", config: `{"enabled":true}`, choice: enableCheckpointRemoteChoice{offered: true}, want: "Keeping checkpoint destination: origin (automatic)"},
		{name: "explicit already set", config: `{"enabled":true,"strategy_options":{"checkpoint_push_remote":"origin"}}`, choice: enableCheckpointRemoteChoice{name: "origin"}, want: "Keeping checkpoint destination: origin (configured)", wantUnchangedLine: true},
		{name: "invalid", config: `{"strategy_options":{"checkpoint_push_remote":"missing"}}`, want: "Checkpoint sync remains disabled:"},
		{name: "dedicated untouched", config: `{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"org/checkpoints"}}}`},
		{name: "dedicated requested", config: `{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"org/checkpoints"}}}`, choice: enableCheckpointRemoteChoice{dedicatedRequested: true}, want: dedicatedURL},
		{name: "invalid with dedicated", config: `{"strategy_options":{"checkpoint_push_remote":"missing","checkpoint_remote":{"provider":"github","repo":"org/checkpoints"}}}`, want: "Dedicated checkpoint destination for pushes to origin: " + dedicatedURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := setupTestRepo(t)
			testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/repo.git")
			writeSettings(t, tc.config)
			var output bytes.Buffer
			choice := tc.choice
			choice.report(t.Context(), &output, nil)
			if tc.want == "" {
				require.Empty(t, output.String())
				return
			}
			require.Contains(t, output.String(), tc.want)
			require.Equal(t, tc.wantUnchangedLine, strings.Contains(output.String(), "No checkpoint destination settings changed."))
			if tc.name == "invalid with dedicated" {
				require.NotContains(t, output.String(), "sync remains disabled")
			}
		})
	}
}

// A configured repo never opens the picker on a bare re-enable, even on a
// terminal: "Keep current destination" writes nothing, so it would come back
// every run. Under ENTIRE_TEST_TTY=1 a picker would fail opening the TTY, so
// a clean exit is the assertion.
func TestEnableCheckpointPushRemote_ReenableDoesNotPrompt(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "1")
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/repo.git")
	testutil.RunGit(t, dir, "remote", "add", "fork", "https://github.com/me/repo.git")
	writeSettings(t, `{"enabled":true}`)
	cmd := newEnableCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{})
	require.NoError(t, cmd.Execute())
	require.NotContains(t, output.String(), "checkpoint destination selection")
	require.NotContains(t, output.String(), "Keeping checkpoint destination")
	require.NotContains(t, output.String(), "No checkpoint destination settings changed.")
	require.Contains(t, output.String(), "This repo has 2 remotes (fork, origin)")
	require.NoFileExists(t, EntireSettingsLocalFile)
}

func TestEnableCheckpointPushRemote_NoBootstrap(t *testing.T) {
	dir := setupTestDir(t)
	cmd := newEnableCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--checkpoint-push-remote", "origin", "--init-repo", "--yes"})
	require.ErrorContains(t, cmd.Execute(), "git repository")
	require.NoDirExists(t, filepath.Join(dir, ".git"))
}

func TestEnableCheckpointPushRemote_FanoutRendering(t *testing.T) {
	t.Parallel()
	topology := remoteTopology{primaryIsRefs: true, destinations: []remoteDestination{{name: "fork", pushURLs: []string{"https://github.com/me/repo.git", "https://github.com/me/other.git"}}}}
	var output bytes.Buffer
	topology.describeCheckpointDestination(&output, "Destinations:")
	require.Equal(t, "Destinations:\n  Remote \"fork\" pushes to 2 URLs:\n    → https://github.com/me/repo.git\n      https://github.com/me/other.git\n    Checkpoints go to the first URL only; the others receive your code but\n    no session history. Clone that first repository to resume elsewhere.\n  To pin one repository for checkpoints, set checkpoint_remote in\n  .entire/settings.json (or .entire/settings.local.json to keep it to this clone).\n", output.String())
}

func TestEnableCheckpointPushRemote_MixedDedicatedAlternatives(t *testing.T) {
	for _, additionalOrdinary := range []bool{false, true} {
		t.Run(strconv.FormatBool(additionalOrdinary), func(t *testing.T) {
			dir := setupTestRepo(t)
			testutil.RunGit(t, dir, "remote", "add", "a", "https://github.com/other/app.git")
			testutil.RunGit(t, dir, "remote", "add", "b", "https://github.com/org/app.git")
			if additionalOrdinary {
				testutil.RunGit(t, dir, "remote", "add", "c", "https://github.com/other/fork.git")
			}
			writeSettings(t, `{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"org/checkpoints"}}}`)
			called := false
			choice, err := prepareEnableCheckpointRemoteSelection(t.Context(), EnableOptions{}, false, func(_ context.Context, options []huh.Option[string]) (string, error) {
				called = true
				for _, option := range options {
					require.NotEqual(t, "b", option.Value)
				}
				return "c", nil
			})
			require.NoError(t, err)
			require.Equal(t, additionalOrdinary, called)
			require.NoError(t, choice.persist(t.Context()))
			var output bytes.Buffer
			choice.report(t.Context(), &output, nil)
			if !called {
				require.Empty(t, output.String(), "an untouched healthy destination is not reported")
				return
			}
			require.Contains(t, output.String(), "Pushes to b still upload checkpoints to the dedicated destination: https://github.com/org/checkpoints.git")
		})
	}
}

func TestEnableCheckpointPushRemote_RejectPushURLOnlyBeforeWrites(t *testing.T) {
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/app.git")
	testutil.RunGit(t, dir, "config", "remote.broken.pushurl", "https://github.com/org/other.git")
	writeSettings(t, `{"enabled":true}`)
	before, err := os.ReadFile(EntireSettingsFile)
	require.NoError(t, err)
	cmd := newEnableCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--checkpoint-push-remote", "broken"})
	require.ErrorContains(t, cmd.Execute(), "configured Git remote")
	after, err := os.ReadFile(EntireSettingsFile)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NoFileExists(t, EntireSettingsLocalFile)
	require.NoFileExists(t, filepath.Join(dir, ".git", "hooks", "pre-push"))
}

func TestEnableCheckpointPushRemote_PickerExcludesPushURLOnly(t *testing.T) {
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/app.git")
	testutil.RunGit(t, dir, "config", "remote.broken.pushurl", "https://github.com/org/other.git")
	choice, err := prepareEnableCheckpointRemoteSelection(t.Context(), EnableOptions{}, false, func(context.Context, []huh.Option[string]) (string, error) {
		t.Fatal("no eligible alternative: picker must not open")
		return "", nil
	})
	require.NoError(t, err)
	require.NoError(t, choice.persist(t.Context()))
	require.NoDirExists(t, filepath.Join(dir, ".entire"))
}

func TestEnableCheckpointPushRemote_DedicatedFlagReport(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(disabled), func(t *testing.T) {
			dir := setupTestRepo(t)
			testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/app.git")
			choice, err := prepareEnableCheckpointRemoteSelection(t.Context(), EnableOptions{CheckpointRemote: "github:org/checkpoints"}, false, nil)
			require.NoError(t, err)
			writeSettings(t, `{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"org/checkpoints"},"push_sessions":`+strconv.FormatBool(!disabled)+`}}`)
			var output bytes.Buffer
			choice.report(t.Context(), &output, nil)
			require.NotContains(t, output.String(), "No checkpoint destination settings changed.")
			require.NotContains(t, output.String(), "Keeping checkpoint destination:")
			if disabled {
				require.Contains(t, output.String(), "Checkpoint pushing remains disabled.")
			} else {
				require.Contains(t, output.String(), "https://github.com/org/checkpoints.git")
			}
		})
	}
}
func TestEnableCheckpointPushRemote_DeferredSelection(t *testing.T) {
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/app.git")
	testutil.RunGit(t, dir, "remote", "add", "fork", "https://github.com/me/app.git")
	choice := &enableCheckpointRemoteChoice{pending: true}
	called := false
	err := choice.selectAfterAgents(t.Context(), EnableOptions{}, func(_ context.Context, options []huh.Option[string]) (string, error) {
		called = true
		require.Len(t, options, 2)
		return "fork", nil
	})
	require.NoError(t, err)
	require.True(t, called)
	require.Equal(t, "fork", choice.name)
	require.False(t, choice.pending)
	require.NoFileExists(t, EntireSettingsFile)
	require.NoFileExists(t, EntireSettingsLocalFile)
}

func TestEnableCheckpointPushRemote_DeferredCancellation(t *testing.T) {
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/app.git")
	testutil.RunGit(t, dir, "remote", "add", "fork", "https://github.com/me/app.git")
	choice := &enableCheckpointRemoteChoice{pending: true}
	err := choice.selectAfterAgents(t.Context(), EnableOptions{}, func(context.Context, []huh.Option[string]) (string, error) {
		return "", context.Canceled
	})
	require.Error(t, err)
	require.Empty(t, choice.name)
	require.NoFileExists(t, EntireSettingsFile)
	require.NoFileExists(t, EntireSettingsLocalFile)
	require.NoFileExists(t, filepath.Join(dir, ".git", "hooks", "pre-push"))
}
func TestEnableCheckpointPushRemote_RepairSavedPushURLOnly(t *testing.T) {
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/app.git")
	testutil.RunGit(t, dir, "config", "remote.broken.pushurl", "https://github.com/me/app.git")
	writeSettings(t, `{"strategy_options":{"checkpoint_push_remote":"broken"}}`)
	called := false
	choice, err := prepareEnableCheckpointRemoteSelection(t.Context(), EnableOptions{}, false, func(_ context.Context, options []huh.Option[string]) (string, error) {
		called = true
		require.Len(t, options, 2)
		require.Contains(t, options[0].Key, "broken")
		return defaultMirrorRemote, nil
	})
	require.NoError(t, err)
	require.True(t, called, "an ineligible saved remote must be repairable")
	require.Equal(t, "origin", choice.name)
}

func TestEnableCheckpointPushRemote_HintRequiresEligibleAlternative(t *testing.T) {
	for _, eligible := range []bool{false, true} {
		t.Run(strconv.FormatBool(eligible), func(t *testing.T) {
			dir := setupTestRepo(t)
			testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/app.git")
			testutil.RunGit(t, dir, "config", "remote.broken.pushurl", "https://github.com/me/app.git")
			if eligible {
				testutil.RunGit(t, dir, "remote", "add", "fork", "https://github.com/me/fork.git")
			}
			var output bytes.Buffer
			(&enableCheckpointRemoteChoice{offered: true}).report(t.Context(), &output, nil)
			require.Equal(t, eligible, strings.Contains(output.String(), "To choose another destination"))
		})
	}
}

func TestEnableCheckpointPushRemote_IneligibleSavedReport(t *testing.T) {
	dir := setupTestRepo(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/org/app.git")
	testutil.RunGit(t, dir, "config", "remote.broken.pushurl", "https://github.com/me/app.git")
	writeSettings(t, `{"strategy_options":{"checkpoint_push_remote":"broken"}}`)
	var output bytes.Buffer
	(&enableCheckpointRemoteChoice{}).report(t.Context(), &output, nil)
	require.NotContains(t, output.String(), "Checkpoints will be uploaded")
	require.Contains(t, output.String(), "--checkpoint-push-remote")
}
