package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Not parallel: remote inspection resolves Git state from CWD and uses t.Chdir.
func TestCheckpointDestinationNote_ExplicitSelection(t *testing.T) {
	for _, tt := range []struct {
		name         string
		selected     string
		multipleURLs bool
		wantChoice   bool
	}{
		{name: "explicit fork", selected: "fork"},
		{name: "default still offers choice", wantChoice: true},
		{name: "missing selection is not resolved", selected: "gone", wantChoice: true},
		{name: "explicit fork retains multiple URL warning", selected: "fork", multipleURLs: true},
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
			testutil.WriteCheckpointPushRemoteSetting(t, dir, tt.selected)
			if tt.multipleURLs {
				testutil.RunGit(t, dir, "remote", "set-url", "--add", "--push", "fork", "https://example.com/contributor/app.git")
				testutil.RunGit(t, dir, "remote", "set-url", "--add", "--push", "fork", "https://example.com/contributor/backup.git")
			}
			t.Chdir(dir)

			var out bytes.Buffer
			printCheckpointDestinationNote(context.Background(), &out, "Checkpoint destination: REVIEW")
			got := out.String()
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
