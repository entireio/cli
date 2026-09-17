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

### Credential Store Selection (keyring or tokens.json?)

`internal/entireclient/tokenstore` picks the backend once per process, in
`resolveBackend`, from three production inputs in strict precedence (the
`go test` temp store slots in after the marker; see below): `ENTIRE_TOKEN_STORE`
when set (`file`, or anything else meaning the keyring; explicit, never falls
back, and a successful write through it is remembered; an explicit keyring write
also removes the superseded copy of that credential from the default-path
`tokens.json`, so a plaintext bearer does not linger for the fallback to
re-adopt), then the remembered
preference in `<config dir>/token_store.json` (`preference.go` — it records
**the backend that holds the freshest credential**: an explicit write records
it, and the Linux fallback records it once the file store proves it holds the
credential (Get, Set or Delete — never after a keyring timeout). Reads through
the marker never change it. It is never written while `ENTIRE_TOKEN_STORE_PATH`
is set, because the marker cannot carry a path and would point later processes
at the default one), then the platform default. On Linux/BSD the default
keyring is fronted by `fallbackStore` (`fallback.go`): a keyring call that fails
for an availability reason — anything but `ErrNotFound` and Ctrl-C — is retried
on the file store at `FileBackendPath` (`ENTIRE_TOKEN_STORE_PATH` when set, else
`tokens.json` in the config dir), and once the file store proves it holds the
credential it is adopted, announced once on stderr, and remembered — except
after a keyring *timeout on a write*, which is adopted for this process only:
the abandoned write may still complete once the keyring answers, and a marker
would orphan that copy (a timed-out read or delete orphans nothing and is
remembered like any other availability failure). Once the keyring has answered in a process (a
success or an `ErrNotFound`), no later call in that process falls back: login
writes the refresh and access slots as two calls, and falling back on only the
second would split one login across two stores. A fallback whose file
operation also fails wraps `ErrFileStoreFailed`, which `withHeadlessStoreHint` and
`storeReadError` check so they never recommend the store that just failed
(they point at `ENTIRE_TOKEN_STORE_PATH` instead). macOS and Windows never
fall back: there the keyring is always present, so a failure is a denied prompt
or a locked store, and a plaintext file must not be the silent answer to either.

Three consequences for tests. The marker is process-visible state in the
per-user config dir, so any test that resolves the backend with the variable
unset, or asserts on `FileBackendSelected()`/`BackendDescription()`, must
isolate `ENTIRE_CONFIG_DIR`. The decision in `resolveBackend` is pure over
`backendInputs` (constructing a file store still reads the path environment)
precisely so the keyring branches can be tested at all: under `go test` the
`testdirs` store sits in front of them and `resolveBackendLocked` never reaches
them. And `resolveBackendLocked` drops an explicit non-`file`
`ENTIRE_TOKEN_STORE` when it detects a test process, so a `keyring` exported in
a developer's shell cannot route a test's writes to the real OS keyring; only
the pure resolver honours it, and only its own tests exercise that branch.
Do not spell the config-dir string resolver's call in a comment in any non-test
`.go` file in the repository (the guard's pathspec excludes `_test.go`; two
named non-consumers are skipped in the guard's own code, not by the pathspec):
the consumer ledger guard is a `git grep` and reads a mention as a call.

A Get that misses in both stores returns the keyring error, not `ErrNotFound`,
and `auth status` renders it as "could not be read from …" rather than "Not
logged in" (`statusTarget.storeErr`). A Delete that misses in both stores
returns `ErrNotFound` silently, so `logout` can still remove a context on a
machine whose keyring has vanished; the store cannot tell a logout from login's
best-effort clear of a stale slot, so it is `logout` that warns, from
`statusTarget.storeErr`, when the token could not be read: revocation was
skipped and any copy in that store was not removed.

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
