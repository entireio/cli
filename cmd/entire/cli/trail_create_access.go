package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// repoPermissionRead is the lowest tier the repo-meta route reports.
const repoPermissionRead = "read"

// trailCreateRefusal explains a create the trail routes would refuse with
// their one collapsed 404 "repository not found". The repo-meta route tells
// "trails off" apart from "no write access", so the check runs before any
// branch is pushed for the trail. A meta read that fails explains nothing:
// nil lets the create proceed to the server's own answer.
func trailCreateRefusal(ctx context.Context, client *api.Client, repoID, forge, owner, repo string) error {
	if strings.TrimSpace(repoID) == "" {
		return nil
	}
	meta, err := client.FetchRepoMeta(ctx, repoID)
	if err != nil {
		logging.Debug(ctx, "trail create: repo meta unavailable", "error", err)
		return nil
	}
	ref := forge + "/" + owner + "/" + repo
	switch {
	case !meta.TrailsEnabled:
		return fmt.Errorf("trails are not enabled for %s\nhint: a repo admin can turn them on under Settings > Trails%s", ref, repoSettingsHint(forge, owner, repo))
	case meta.Permission == repoPermissionRead:
		return fmt.Errorf("you need write access to %s to create a trail", ref)
	}
	return nil
}

// repoSettingsHint renders " (<web-origin>/<forge>/<owner>/<repo>/settings)",
// or "" when no web origin is known. The data API base doubles as the web
// origin, as in trailWebURL.
func repoSettingsHint(forge, owner, repo string) string {
	base, err := auth.DataBaseURL()
	if err != nil {
		return ""
	}
	return " (" + strings.TrimRight(base, "/") + "/" + forge + "/" + owner + "/" + repo + "/settings)"
}
