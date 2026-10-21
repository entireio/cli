package cli

import (
	"context"
	"io"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// currentRepoRefTimeout bounds currentRepoRef's control-plane lookup. The
// lookup is best-effort decoration (recap degrades to personal-only without
// it), so a stalled core must not hang the command — mirror cellResolveTimeout.
const currentRepoRefTimeout = 5 * time.Second

// runAuthenticatedActivityAPI runs fn with a client for the caller's home
// entire-api cell, which serves the /me/* endpoints the activity command calls.
// There is no data-API fallback: the BFF answers /me/* by proxying to that same
// home cell, so a caller the cell path cannot serve gets nothing better there.
// Client-construction failures go through renderDataAPIAuthError, which turns
// ErrNotLoggedIn into the login hint and silences the caller's own cancellation.
func runAuthenticatedActivityAPI(ctx context.Context, errW io.Writer, insecureHTTP bool, fn func(context.Context, *api.Client) error) error {
	client, err := auth.NewEntireAPICellClient(ctx, insecureHTTP, nil)
	if err != nil {
		return renderDataAPIAuthError(ctx, errW, "", err)
	}
	return fn(ctx, client)
}

// currentRepoRef best-effort resolves the current repo (its "origin" remote)
// to the ULID entire-api uses for repo-scoped params — recap's /me/recap?repo=
// — plus the human owner/repo slug for display, from a single remote
// resolution (the caller needs both; resolving twice would double the git and
// control-plane work). The ULID is the repo's processing placement id: a repo
// mirrored in several regions has a different id per placement, and only the
// processing one holds its data. Any failure returns "", "" — recap then shows
// the personal side only rather than erroring.
func currentRepoRef(ctx context.Context) (repoID, repoSlug string) {
	ctx, cancel := context.WithTimeout(ctx, currentRepoRefTimeout)
	defer cancel()

	forge, owner, repo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil || owner == "" || repo == "" {
		return "", ""
	}
	placement, err := resolveForgeRepoCellPlacement(ctx, forge, owner, repo)
	if err != nil || placement.RepoID == "" {
		return "", ""
	}
	return placement.RepoID, owner + "/" + repo
}
