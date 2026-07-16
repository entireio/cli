package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
)

// currentRepoRefTimeout bounds currentRepoRef's control-plane lookup. The
// lookup is best-effort decoration (recap degrades to personal-only without
// it), so a stalled core must not hang the command — mirror cellResolveTimeout.
const currentRepoRefTimeout = 5 * time.Second

// runAuthenticatedActivityAPI runs fn with an authenticated client for the
// activity/recap surface. It prefers the caller's home entire-api cell (the same
// shared client the experts commands use), which serves the /me/* endpoints
// these commands call.
//
// Cell routing is a best-effort upgrade: any failure building the cell client —
// the region has no cell yet (ErrNoCellForJurisdiction), not logged in, or a
// discovery/exchange error — falls back to the data API, which also serves
// /me/* and yields the canonical auth errors (e.g. the "not logged in" hint).
// This keeps the migration transparent and non-regressive; non-obvious
// fallbacks are logged for diagnosis. Both backends expose the same /me/* paths,
// so fn is agnostic to which client it receives.
func runAuthenticatedActivityAPI(ctx context.Context, errW io.Writer, insecureHTTP bool, fn func(context.Context, *api.Client) error) error {
	client, err := auth.NewEntireAPICellClient(ctx, insecureHTTP, nil)
	if err != nil {
		logCellClientFallback(ctx, err)
		return runAuthenticatedDataAPI(ctx, errW, insecureHTTP, fn)
	}
	return fn(ctx, client)
}

// logCellClientFallback records, at debug, that an activity/recap command fell
// back from the entire-api cell to the data API. The expected cases — the
// region has no cell yet, or the caller isn't logged in — aren't logged: they
// are normal during rollout and on first use, not diagnosable failures.
func logCellClientFallback(ctx context.Context, err error) {
	if errors.Is(err, auth.ErrNoCellForJurisdiction) || errors.Is(err, auth.ErrNotLoggedIn) {
		return
	}
	logging.Debug(ctx, "activity/recap: entire-api cell client unavailable, using data API", "error", err.Error())
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
