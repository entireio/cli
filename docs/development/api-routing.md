# API routing

Control-plane precedence and jurisdictional data-plane routing. Read before changing authentication targets or API request routing.

Repository paths in code spans are relative to the repository root unless stated otherwise.

### Control-Plane Core Resolution (which core am I talking to?)

Control-plane commands dial one of three cores: the active context's
(`coreapi.New`), a specific cluster's (`coreapi.NewForCluster`), or — when
`ENTIRE_TOKEN` is set — the env token's `aud` (the bypass inside `New`/
`NewForCluster`). This precedence lives **only** inside `coreapi`; nothing else
re-derives it.

**To display which core a request uses, ask the client: `client.CoreOrigin()`.**
It returns whatever was actually wired in, so the shown core can never diverge
from where the request goes. **Do NOT** re-resolve with
`auth.ResolveControlPlaneTarget()` for display — it only knows the active
context and silently ignores both `ENTIRE_TOKEN` and the cluster case, so it can
name a core the request never touches (this was a real bug in the `mirror list`
banner; see `repo_mirror.go` and `coreapi.Client.CoreOrigin`).

When a command resolves auth *outside* a `coreapi.Client` (e.g. `entire auth
status`, which builds its own `/me` client), it must apply the same
env-token-first precedence itself — see `resolveAuthStatusTarget` /
`resolveEnvTokenStatusTarget` in `auth.go`, which branch on
`auth.EnvTokenVar` before falling back to the active context. `logout` is the
deliberate exception: it manages a *stored* login session, which an ephemeral
env token has none of, so it stays on the active context.

### Entire-API Cell Routing (which cell does a data-plane request go to?)

The data plane (entire-api) is deployed per jurisdiction; a repo placement
lives in exactly one cell, user `/me/*` activity is consolidated in the
caller's home cell, and no server-side cross-cell aggregator exists. The CLI
therefore has exactly three routing shapes, mirroring the entire.io BFF:

- **Repo-scoped → one cell**: `resolveRepoCellTarget` (`cell_target.go`) maps
  a repo (ULID or owner/repo) to the cell hosting it — via `GetRepo`'s
  `ClusterHost` for a ULID, or via the control plane's consolidated repos
  index (`ListRepos`) for owner/repo, resolved to the repo's PROCESSING
  placement (`primaries.processing`), not just any active mirror, since a
  repo can be mirrored in several regions but only one placement holds its
  actual data. NOT best-effort: any failure (not onboarded, no/failed/
  suspended processing placement, control-plane error, timeout) returns an
  error instead of falling back to home-jurisdiction routing — a wrong-region
  "success" is worse than a command failure for repo-scoped data. Used by
  trails (`NewAuthenticatedEntireAPICellClient` in `api_client.go`) and by
  `experts --repo <ulid>`. `resolveRepoCellPlacement` performs the same lookup
  for callers that also need the placement's id (repo_id) alongside its cell —
  used by cross-repo checkpoint reads (`explain --repo`, `explain_repo.go`) and
  by `experts --repo owner/repo`, which sends that placement id to entire-api
  instead of re-deriving it from a data-plane repo listing.
- **User-scoped `/me` → home cell, never fan out**:
  `auth.NewEntireAPICellClient(ctx, insecure, nil)` routes by the
  `home_jurisdiction` JWT claim; activity/recap use it with a data-API
  fallback (`runAuthenticatedActivityAPI` in `entireapi_client.go`).
- **Repo-set queries → fan out and merge client-side**: `cell_fanout.go` —
  `groupReposByCell` (repo index → per-cell groups; the catalog join key is
  `ClusterSlug`↔`Cluster.Slug`, NOT the cell name, which the catalog does not
  expose), `resolveCellBaseURLs`, and `fanOutCells` (parallel per-cell calls,
  per-cell timeout, partial failures isolated per slot). Merge semantics stay
  with the command. Each repo routes to exactly ONE placement — its home,
  picked by `routedRepoPlacement` (the elected `primaries.processing`, else
  the canonical row-ID convention). Mirrors are never searched: they are
  replicated copies indexed under their own namespaces, so an extra leg
  returns duplicate and stale rows, and diverges from the web
  (ENT-1672/ENT-1776). Unlike the repo-scoped resolver above, this one fails
  SOFT — a home placement that is not ready is skipped and reported
  (`reportableSkippedRepos`: pinned requests always warn, broad ones only
  when the skips left no cell to query), never substituted with a ready
  mirror.

Token rule: identity tokens are **per-jurisdiction, not per-cell**. Multi-cell
callers must build one `auth.CellClientFactory`
(`NewEntireAPICellClientFactory`) per operation — it resolves the login
subject once and mints at most one token per jurisdiction. `fanOutCells` does
this automatically; do not call `NewEntireAPICellClient` in a loop.
