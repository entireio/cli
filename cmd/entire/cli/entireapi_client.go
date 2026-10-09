package cli

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/internal/coreapi"
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

// forgeToMirrorProvider maps a gitremote forge identifier (e.g. "gh") to the
// upstream provider the control plane records mirrors under (e.g. "github").
// entire-api routing only supports GitHub mirrors today.
func forgeToMirrorProvider(forge string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(forge)) {
	case "gh", mirrorCloneProviderGitHub:
		return mirrorCloneProviderGitHub, true
	default:
		return "", false
	}
}

// currentRepoRef best-effort resolves the current repo (its "origin" remote)
// to the ULID entire-api uses for repo-scoped params — recap's /me/recap?repo=
// — plus the human owner/repo slug for display, from a single remote
// resolution (the caller needs both; resolving twice would double the git and
// control-plane work). entire.io/api documents the mirror id as exactly that
// repo_id (repo_id = mirror_repos.id), and the CLI already lists mirrors via
// the control plane, so no extra resolution is needed. Any failure returns
// "", "" — recap then shows the personal side only rather than erroring.
func currentRepoRef(ctx context.Context) (repoID, repoSlug string) {
	ctx, cancel := context.WithTimeout(ctx, currentRepoRefTimeout)
	defer cancel()

	forge, owner, repo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil || owner == "" || repo == "" {
		return "", ""
	}
	provider, ok := forgeToMirrorProvider(forge)
	if !ok {
		return "", ""
	}
	c, err := coreapi.New()
	if err != nil {
		return "", ""
	}
	mirrors, err := listMirrorsForRepo(ctx, c, provider, strings.ToLower(owner), repo)
	if err != nil {
		return "", ""
	}
	repoID = firstActiveRepoID(mirrors)
	if repoID == "" {
		return "", ""
	}
	return repoID, owner + "/" + repo
}

// firstActiveRepoID returns the id of the repo's first active mirror (the repo
// id is stable across a repo's placements, so any active one serves). Archived
// and failed/suspended placements are skipped — they can't answer for the repo.
func firstActiveRepoID(mirrors []coreapi.Mirror) string {
	for i := range mirrors {
		if !isActiveMirror(mirrors[i]) {
			continue
		}
		if id := strings.TrimSpace(mirrors[i].MirrorId); id != "" {
			return id
		}
	}
	return ""
}
