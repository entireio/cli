# API routing

Control-plane precedence and jurisdictional data-plane routing. Read before changing authentication targets or API request routing.

Repository paths in code spans are relative to the repository root unless stated otherwise.

### Credential-bearing redirects and replicas

- **Construct clients with `httpclient.New`** (`internal/entireclient/httpclient`). It limits redirect chains and refuses HTTPS → HTTP, including HTTPS → loopback and HTTP → HTTPS → HTTP. A source guard rejects bare clients, zero-value declarations and package-level HTTP helpers outside documented exceptions.
- **Preserve stricter policies.** The API client (also used by cloud dispatch) refuses cross-host redirects and insecure absolute endpoints. The remote-helper proxy restricts bearer forwarding to its cluster. Well-known discovery refuses **all** redirects at the fetch boundary; its response is a trust root. Auth-go separately protects OAuth exchanges and 421 routing, not the outer control-plane client's ordinary HTTP redirects.
- **Wrap injected clients with `WithSecureRedirects`.** It copies the supplied client (nil means `http.DefaultClient`). The client's own callback runs first and can only add restrictions; it must not call `CheckSecureRedirect` itself. `http.ErrUseLastResponse` is returned unchanged, so the cold replica probe can read a 3xx's headers even with an HTTP Location. Otherwise a shared-policy rejection wins over the callback's own error, so a downgrade always reports `ErrInsecureRedirect`. If the callback permits following, both the proposed and final URLs must pass validation; the callback cannot undo an HTTPS upgrade. Inject a transport via `api.NewClientWithTransport` in tests, not by replacing `http.DefaultTransport`, and never replace a constructed client's `CheckRedirect` or `Transport` (a source guard lists the audited exceptions).
- **Do not rewrite schemes beneath the redirect policy for other hosts.** The policy only sees the request URL, so a transport that downgrades to HTTP hides the downgrade from it. Plain-HTTP data-API discovery rewrites only the data origin's own host; the cluster listing at the discovered core, which carries the login JWT, keeps its scheme.
- **Classify policy rejections, then fail the node.** Downgrades match `httpclient.ErrInsecureRedirect` and `ErrRedirectPolicy`; direct insecure replica requests return `transport.ErrInsecureReplica`. Both match `httpclient.ErrRequestPolicy` via `errors.Is`. A rejected hop is never sent. The proxy drops the rejecting node like an unreachable one (removed from the in-memory set, replica cache invalidated) and tries the remaining eligible replicas; keeping it would leave it as the only POST target once the entry URL answers info/refs itself. If every cached replica fails, including on policy, the warm path still falls back to the HTTPS entry URL. After an entry-domain redirect, a policy error is returned without following the server's Location. A final HTTP 507 retains its storage error, and skipped nodes do not replace server errors.
- **A redirect loop is a node failure, not a policy rejection.** `httpclient.ErrTooManyRedirects` does not match `ErrRequestPolicy`: the looping node is marked failed, which invalidates the replica cache, and the request falls back as for any unreachable node.
- **Require HTTPS for production replicas.** Validate cache entries, headers, Location targets, response-URL fallback, and requests before attaching auth. `replicas.Resolve` always creates an HTTPS entry; `SkipTLS` does not authorize plaintext. Directly constructed loopback-HTTP proxies are a **test-only** compatibility mode, disabled once that proxy attempts HTTPS.

Keep flow-level regression tests at client wiring boundaries. Recording transports must keep host and port identical when testing a scheme change, and assert that a rejected destination receives no request.

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

A service account secret (`ENTIRE_TOKEN=ent_svc_…`) has no `aud`. Every reader
that needs the bearer or its claims goes through `auth.ResolveEnvToken` (via
`auth.ParseEnvToken`, or `ResolveEnvTokenContext` in `git-remote-entire`).
`auth.LocalIdentityCacheKey` is the one exception: it keys a secret on the
exchange Core and the service account ID, so a cache lookup needs no network.
Readers that only test whether `ENTIRE_TOKEN` is set need nothing. A v1
secret is `ent_svc_<juri>_<account ULID>_<secret ULID>_<random><check>` and is
validated offline, checksum included, before anything is sent
(`parseServiceAccountSecret`). The secret lives in the regional database of
its jurisdiction, so the exchange sends it to `https://<juri>.auth.entire.io`
and nowhere else: there is no override. Against a non-production Core,
exchange the secret by hand and put the JWT in `ENTIRE_TOKEN`, which already
works. The exchange, the identity cache key and
`ForgetServiceAccountSecretToken` derive that origin in one place
(`secretExchangeSubject`). It returns the service account JWT, which lives at
most 10 minutes; that JWT's `aud` then drives the precedence above. The
exchanged token must name the secret's `svc_` account as `sub` and that
Core as its only URL `aud`; its own `exp` wins over `expires_in` when earlier
(`checkExchangedToken`, applied again to every cache read). It is cached under
the user cache root, keyed by a hash of the Core and the secret, and reused
until 2 minutes before it expires. On a miss the exchange and write-back run
under a per-entry file lock, which `ForgetServiceAccountSecretToken` also
takes, so parallel commands share one exchange; a lock that cannot be taken
falls back to an unshared exchange rather than failing auth. The exchange never follows a
redirect, because a 307/308 would resend the secret in the body. Because that
token lives minutes, a client built once must not hold it: `coreapi`, the data
API client (`auth.DataAPI.Client`) and cell clients take `auth.EnvTokenSource`,
which asks the cache for every request (renewing near expiry) and drops the
cached token on a 401 that names it, so a long-running command such as `entire
trail watch` keeps working and a refused token is not reused.
`git-remote-entire` does the same through `secretCreds`. Readers that need only
the origin use `auth.EnvTokenCoreURL`, which derives a secret's core offline
instead of exchanging it. `entire auth
status --json` reports `credential_source: sa_secret` for it. Never send the
secret itself as a bearer, and never name it in an error.

When a command resolves auth *outside* a `coreapi.Client` (e.g. `entire auth
status`, which builds its own `/me` client), it must apply the same
env-token-first precedence itself — see `resolveAuthStatusTarget` /
`resolveEnvTokenStatusTarget` in `auth.go`, which branch on
`auth.EnvTokenVar` before falling back to the active context. `logout` is the
deliberate exception: it manages a *stored* login session, which an ephemeral
env token has none of, so it stays on the active context.

### Repository discovery

The dispatch wizard and `search --repo` completion use the control plane's
`ListRepos` index, sorted by `last_activity_at` descending, through
`listCheckpointRepoIndex` for the wizard. Its walk is bounded to 5,000 entries
and ten seconds, with `hasCheckpoints=true` so empty repos do not consume the
budget; truncation logs a warning and keeps the partial catalogue.
Completion instead uses `listCompletionRepoIndex`: one page of up to 100 entries,
a one-second request timeout, and server-side checkpoint/name filters. It never
follows the cursor and filters returned slugs by the typed prefix. The server's
name filter is sent only for text after a complete `gh/` or `et/` prefix, so
partial forge prefixes do not hide valid suggestions. Both omit
repos with no checkpoints and qualify names by provider. The wizard derives READY
placement jurisdictions from the same walk and retains its local-repo fallback;
completion silently falls back to `*` on errors. Code-search filters resolve these qualified
slugs against the entry's provider, accepting bare or already-qualified index
names without crossing forge namespaces. Two-component names whose owner/project
is `gh` or `et` retain that segment. ID filters trim surrounding whitespace and
retain precedence over name matching. Duplicate normalized slugs preserve all
matching search entries (deduplicated by ID); wizard jurisdictions are merged
and picker suggestions deduplicated, preserving the first spelling/order.
Do not use the removed BFF
`GET /api/v1/repositories` route.

### Entire-API Cell Routing (which cell does a data-plane request go to?)

The data plane (entire-api) is deployed per jurisdiction; a repo placement
lives in exactly one cell, user `/me/*` activity is consolidated in the
caller's home cell, and no server-side cross-cell aggregator exists. The CLI
uses the following routing shapes, mirroring the entire.io BFF:

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
  `experts --repo <ulid>`. `resolveRepoCellPlacement` performs the same lookup
  for callers that also need the placement's id (repo_id) alongside its cell —
  used by trails (`newTrailAPIClient` in `api_client.go`, through the
  forge-qualified `resolveForgeRepoCellPlacement`), by cross-repo checkpoint
  reads (`explain --repo`, `explain_repo.go`; the same placement id keys both
  `/checkpoints/{id}` and the commit→checkpoint resolution at
  `/commits/{sha}/checkpoints`) and by `experts --repo owner/repo`, which sends
  that placement id to entire-api
  instead of re-deriving it from a data-plane repo listing.
- **Project-scoped trails → assigned project cell**:
  Core resolves the required `--project`; list uses
  `GET /api/v1/trails?projectId=<ID>` at its assigned API URL, and a numeric
  selector is one `GET /api/v1/{host}/{project}/trails/{number}` there. `--repo` only
  filters within that project. No fanout or fallback; server cursors pass through.
  See [Project trails CLI](../architecture/project-trails-cli.md).
- **User-scoped `/me` → home cell, never fan out**:
  `auth.NewEntireAPICellClient(ctx, insecure, nil)` routes by the
  `home_jurisdiction` JWT claim. Activity and recap use it with no data-API
  fallback (`runAuthenticatedActivityAPI` in `entireapi_client.go`,
  `newRecapClient` in `recap.go`): the BFF serves `/me/*` by proxying to the
  same home cell, so it cannot answer a caller the cell path cannot.
  One exception: `/me/project-trails` (`trail list --mine`,
  `project_trail_list_mine.go`). A project trail lives in its project's cell,
  so each cell knows only the caller's trails homed there. Like the web
  sidebar (`me-project-trails.ts`), the CLI asks every jurisdiction's default
  cluster from the catalog through `fanOutCells`, keeps the newest copy of
  each trail, and merges by latest activity into one bounded page (per-region
  cursors cannot continue a merged order). Unlike the sidebar, which fails
  the whole request when any region fails, the CLI lists what the other
  regions returned and reports the failed region as partial (status, code,
  and request ID only). A 401 from any region still fails the command, since
  every region is sent the same credentials; a 403 can mean only that the
  caller has no access in that region, so it leaves that region partial.
- **Cloud dispatch → one jurisdiction's cell**: `entire dispatch` builds one
  `CellClientFactory` (`newDispatchCellClient` in `dispatch/mode_cloud.go`)
  and dials the `--jurisdiction` cell, else the home cell. It posts to
  `/me/dispatches`, which answers 202 with a generating run, and polls
  `/me/dispatches/{id}` until the run completes or fails. The cell resolves
  the forge-qualified repos among its own placements and 404s
  `repository not found: …` for the rest. The dispatch wizard reads the home
  jurisdiction from the same factory. Do not use the BFF's
  `POST /api/v1/dispatches/generate`.
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

Token rule: the login JWT is the bearer at every cell. Multi-cell callers must
build one `auth.CellClientFactory` (`NewEntireAPICellClientFactory`) per
operation — it resolves and refreshes the login subject once, however many
cells it dials. `fanOutCells` does this automatically; do not call
`NewEntireAPICellClient` in a loop.
