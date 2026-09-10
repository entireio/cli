package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Not parallel: remote inspection resolves Git state from CWD and uses t.Chdir.
func TestCheckpointDestinationNote_ExplicitSelection(t *testing.T) {
	for _, tt := range []struct {
		name         string
		selected     string
		multipleURLs bool
		otherURLs    bool
		observed     bool
		wantChoice   bool
	}{
		{name: "explicit fork", selected: "fork"},
		{name: "default still offers choice", wantChoice: true},
		{name: "missing selection is not resolved", selected: "gone", wantChoice: true},
		{name: "explicit fork retains multiple URL warning", selected: "fork", multipleURLs: true},
		{name: "explicit fork ignores origin fanout", selected: "fork", otherURLs: true},
		{name: "explicit fork only describes its own fanout", selected: "fork", multipleURLs: true, otherURLs: true},
		{name: "observed fork still explains choice", observed: true, wantChoice: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testutil.IsolateGitConfigEnv(t)
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			testutil.WriteFile(t, dir, "f.txt", "init")
			testutil.GitAdd(t, dir, "f.txt")
			testutil.GitCommit(t, dir, "init")
			testutil.AddRemote(t, dir, "origin", "https://example.com/upstream/app.git")
			testutil.AddRemote(t, dir, "fork", "https://example.com/contributor/app.git")
			if tt.selected != "" {
				testutil.WriteCheckpointPushRemoteSetting(t, dir, tt.selected)
			} else {
				testutil.WriteFile(t, dir, ".entire/settings.json", `{"enabled": true}`)
			}
			if tt.multipleURLs {
				testutil.RunGit(t, dir, "remote", "set-url", "--add", "--push", "fork", "https://example.com/contributor/app.git")
				testutil.RunGit(t, dir, "remote", "set-url", "--add", "--push", "fork", "https://example.com/contributor/backup.git")
			}
			if tt.otherURLs {
				testutil.RunGit(t, dir, "remote", "set-url", "--add", "--push", "origin", "https://example.com/upstream/app.git")
				testutil.RunGit(t, dir, "remote", "set-url", "--add", "--push", "origin", "https://example.com/upstream/backup.git")
			}
			if tt.observed {
				testutil.WriteFile(t, dir, ".git/entire-checkpoint-sync-remotes.json", `{"remotes":["fork"]}`)
			}
			t.Chdir(dir)
			if tt.observed {
				elected, err := strategy.ResolveCheckpointSyncRemote(t.Context())
				if err != nil || elected.Name != "fork" || elected.Source != strategy.SyncRemoteSourceObserved {
					t.Fatalf("expected observed fork election, got %+v, error: %v", elected, err)
				}
			}

			var out bytes.Buffer
			printCheckpointDestinationNote(context.Background(), &out, "Checkpoint destination: REVIEW")
			got := out.String()
			if tt.otherURLs && strings.Contains(got, `Remote "origin"`) {
				t.Errorf("unselected origin must not be described as carrying checkpoints:\n%s", got)
			}
			if choice := strings.Contains(got, "This repo has 2 remotes"); choice != tt.wantChoice {
				t.Errorf("multi-remote choice warning = %v, want %v; output:\n%s", choice, tt.wantChoice, got)
			}
			if tt.multipleURLs {
				if !strings.Contains(got, `Remote "fork" pushes to 2 URLs`) {
					t.Errorf("missing multiple-push-URL warning:\n%s", got)
				}
			} else if !tt.wantChoice && got != "" {
				t.Errorf("valid explicit single-URL selection should need no destination note:\n%s", got)
			}
		})
	}
}
