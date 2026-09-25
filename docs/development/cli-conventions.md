# CLI development conventions

Command implementation details and CLI interaction patterns. For installed command usage, run `entire agent-help`; this reference is for changing the implementation.

Repository paths in code spans are relative to the repository root unless stated otherwise.

### Command Layout

The visible CLI is organized around a set of noun groups plus a small set of
top-level verbs. The groups are the canonical home for each verb. Newer
experimental command families are discoverable through `entire labs` and
their canonical paths are always runnable.

Experimental commands are gated by a build-time visibility flag (the
`cmd/entire/cli/experimental` package): they are shown — grouped under an
"Experimental commands:" help section — in developer and nightly builds, and
hidden in stable release builds. Visibility is toggled by `experimental.Visible`
(default `"true"`), which GoReleaser stamps `"false"` only on stable tags
(`.Prerelease` empty); nightly (`vX.Y.Z-nightly.*`) and local builds leave it at
the default. Register a command as experimental with `experimental.Register(parent,
child)` instead of `parent.AddCommand(child)`. Gating only controls visibility —
the commands are always runnable in every build.

- `session` (alias: `sessions`): `list`, `info`, `tokens`, `stop`, `attach`, `adopt`, `resume`, `current`.
  `resume` with a branch arg switches to it and resumes its session; with no arg
  it opens an interactive picker of stopped sessions (across all worktrees),
  resolving each to its branch and pointing at the owning worktree when the
  branch is checked out elsewhere. Resume keeps an existing local session log
  as-is by default (`--force` overwrites it from the checkpoint).
  `adopt` moves an active session from another repo or worktree into the current
  worktree and resets target-local checkpoint bookkeeping so future commits link
  to the adopted session from the new location.
  `current` and a bare `tokens` answer "which session is running this command?"
  through `strategy.ResolveCallerSession`, not "which state file moved last" —
  see [Resolving the calling session](caller-session-resolution.md#resolving-the-calling-session).
- `checkpoint` (aliases: `cp`, `checkpoints`): `list`, `explain`, `tokens`, `search`.
  `explain` also takes a forge-qualified `--repo` (`gh/<owner>/<name>` or
  `et/<project>/<name>`), the drill-down for a cross-repo `search` hit: it
  reads the checkpoint from that repo's entire-api cell over
  HTTP (`/repos/{repo_id}/checkpoints/{id}` plus `.../transcript/raw`) rather
  than fetching git objects, so a foreign checkpoint never enters this repo's
  object store, ref namespace, or `tokens profile`. It needs a full checkpoint
  ID and a pushed checkpoint; `--commit`, `--session`, `--search-all`, and
  `--generate` are rejected with it, and naming the current repo is a no-op
  that falls through to the local path. See `checkpoint_api_reader.go`
  (`apiCheckpointReader`, which implements the two checkpoint reader tiers and
  deliberately not `Writer`) and `explain_repo.go`.
- `agent`: bare opens the interactive agent selector, plus `list`, `add`, `remove`
- `configure`: bare prints help and a hint pointing at `entire agent`; flags
  manage non-agent settings (telemetry, git-hook installation mode, strategy
  options, summary provider). Agent CRUD lives under `entire agent`.
- `auth`: `login`, `logout`, `status`, `contexts`, `switch`, plus
  `token` (prints the active control-plane bearer to stdout for scripting/curl;
  honors `ENTIRE_TOKEN`, else the refreshed active-context login JWT). `token`
  also takes `--jurisdiction <slug>` (e.g. `us`, `eu`), which instead mints a
  jurisdictional identity token (RFC 8693 exchange, `scope=openid`,
  `aud=<jurisdiction host>`) for that jurisdiction's entire-api cells (e.g.
  `https://aws-us-east-2.api.entire.io/api/v1`), which reject the control-plane
  bearer; it exchanges `ENTIRE_TOKEN` when set (deriving the environment from the
  env token's `aud`), else the active login. `auth status` shows the caller's
  home jurisdiction so the slug is discoverable. `logout` sweeps every saved
  login: one `DELETE /api/auth/tokens` per login server ends every CLI session
  there (core tells them apart by `issuer_client_id`), then the login is
  removed locally. Nothing narrows it: an explicit `--context` is refused
  (`errContextFlagOnLogout`) rather than ignored, since it reads as a request
  to end one login, and `$ENTIRE_CONTEXT` is ignored as ambient state.
  `--everywhere` sends `?scope=all`, which also ends browser and web sessions.
  An older server answers 405; bare `logout` then ends only the bearer's own
  session and `--everywhere` falls back to list + delete-by-id. Each login gets
  its own deadline (`logoutLoginTimeout`); a cancelled context stops the sweep
  with the unreached logins intact, and a failed local removal or an interrupt
  fails the command
- `doctor`: bare runs the scan-and-fix flow, plus `trace`, `logs`, `bundle`
- `cluster`: the control plane's data-plane cluster catalog — `list` only, since
  clusters are provisioned by Entire rather than by users. It renders `GET
  /clusters` (`coreapi.ListClusters`, the same call the mirror wizard and
  `repo mirror list` already make to map slugs to hosts) sorted by region then
  slug. The table's columns are the values other commands take: REGION is the
  jurisdiction slug behind `org create --region` and `project create
  --region`; CLUSTER is the placement slug `repo mirror list --cluster` filters
  on and the key the native-mirror API is addressed by; HOST is the bare public
  host every targeting `--cluster` takes (`repo mirror add`/`remove`, `repo
  clone`, `repo remote add`), reduced through
  `hostFromPublicURL` so a publicUrl that fails validation renders `-` rather
  than a spoofable host. It is also what goes into an `entire://` clone URL and
  what `runCoreForCluster` dials. `--json` is the wire model, `apiUrl` and `isDefault`
  included, plus a synthesized `host` merged into each object
  (`clusterJSON`, via the additive-only `mergeSynthesizedField` that `repo
  create` uses for `remote`): the same validated host the table shows, absent
  rather than dashed when `publicUrl` fails validation, so a script never has
  to re-implement the guard over the raw URL. `apiUrl` is never a table
  column, because the CLI dials the API URL itself. `isDefault` becomes a
  DEFAULT column only when the catalog holds a non-default cluster
  (`clusterTable`): that is the catalog in which a reader needs telling where
  a region falls back to when a command names the region alone, and in a
  catalog with one cluster per region the column would read yes on every
  row. The catalog carries no health, capacity or usage data — nothing
  server-side does — and hidden or decommissioned clusters never reach it.
- `org`: control-plane organization management — `create`, `list`, `get`, `delete`,
  plus `grant` (`add`/`list`/`remove`): org membership for a `provider:handle`
  grantee, roles owner/admin/member (default member)
- `project`: control-plane project management — `create`, `list`, `get`, `delete`,
  plus `grant` (`add`/`list`/`remove`): project access for a `provider:handle`
  grantee, roles reader/writer/admin; both `add` and `remove` take the grantee
  optionally (see the grant-subtree notes below)
- `repo`: control-plane repository lifecycle — `create`, `list --project`,
  `view`, `edit`, `delete`, `clone`, plus the `mirror`, `remote`,
  `visibility`, `protection` and `grant` subtrees (`repo grant` mirrors
  `project grant`, addressing the repo by its `/et/<project>/<repo>` path;
  `grant list` alone also takes a `/gh/` mirror ref). Verb names follow the GitHub CLI where the job is the same (`view`,
  `edit --visibility`, `auth switch`), per the unified-repo-commands proto.
  Git content operations (log, diff, …) are intentionally out of scope.
  **A targeting `--cluster` names a cluster by its public host**
  (`aws-us-east-2.entire.io`, the HOST column of `entire cluster list`) — the
  same coordinate the `entire://` URL carries and `runCoreForCluster` dials. The
  native-mirror API is keyed by the catalog *slug* instead, so the native path
  resolves host → slug through one `GET /clusters` rather than asking for a
  second spelling. `repo mirror list --cluster` is the exception and predates
  this: it is a filter the server resolves, and takes either. Settling the CLI
  on one spelling is worth doing on its own; it is not this change.
  `repo create` takes no cluster at all: a repo's home cluster is the primary
  cell of its owning project's region.
  `protection` (`list`, `add [--server-side-merge-only]`, `remove`) edits a
  native repo's branch-protection rules through core's
  `/repos/{repoId}/branch-protection` resource: `add` and `remove` are one
  PATCH each (`addRules` upserts by ref), never a read-modify-write of the
  list. `add` sends `serverSideMergeOnly` only when the flag was given: the
  server keeps an existing rule's level when it is absent, so re-adding a
  branch without the flag never lowers it and `--server-side-merge-only=false`
  is the explicit way down. A short branch name expands to `refs/heads/`,
  `HEAD` and `refs/...` pass through. The `mirror` subtree is
  server-side (`add`, `list`, `get`, `remove`; `add` and `remove` name clusters
  with `--cluster <host>`, repeatable or comma-separated, and place or tear down
  every named cluster in parallel through one engine — `mirrorTargets` →
  `createMirrors`/`removeMirrors` → a summary table — so a one-shot verb reports
  exactly like the wizard. A failure on one cluster never stops the others; the
  command exits non-zero naming the ones that failed. `add` defaults to one
  fixed cluster without a terminal so scripts stay stable; `remove` has no
  default at all, because which clusters a repo is on is a property of the repo
  and guessing one would delete a copy nobody named — a terminal gets a
  multi-select of the repo's actual placements with nothing pre-ticked, which is
  also the confirmation) and serves **both forges**. A GitHub mirror
  is a clone of an upstream, created through the asynchronous mirror-request
  resource and addressed by `(provider, owner, repo, clusterHost)`. An
  Entire-native mirror is an extra placement of a repo Entire already holds,
  addressed by `(repoId, clusterSlug)` under `/repos/{repoId}/native-mirrors`,
  and three things follow from that: the primary placement is **not** in the
  native-mirror list (so any "where does this repo live" view joins the repo's
  own `clusterSlug` onto it), a replica is **never promoted** (removing one
  tears down that copy alone, and the primary is not in the list to remove),
  and v1 places them **cross-jurisdiction only**. Those last two are why
  `add`/`remove` refuse the repo's own primary cluster and a same-region target
  before writing anything, and why `--cluster` has no default on the native
  path. Pushing and fetching both work through any placement, so a remote
  pointed at one needs no special handling. `list` stays GitHub-only by default; `--forge et|all` opts native rows
  in, classified by the entry's `provider` (falling back to the placements'
  `mirror` flag when the optional field is absent — never the other way round,
  since `mirror` must not decide which placement *routes* a repo). The
  native-mirror routes are home-core-scoped and answer 421 for a repo in another
  jurisdiction, which `coreapi`'s transport follows and re-authenticates on its
  own, so they run on the plain active-context client with no cluster-fronting
  detour. `remote add <remote-name> [repo]` is the whole `remote` subtree: it
  writes one git remote in the *current clone* (local git config only — it
  creates nothing server-side). It serves both forges: for a native repo the
  placements are its primary plus each **ready** mirror. One URL per remote
  either way — a placement serves pushes as well as fetches, so there is no
  split fetch/push remote to maintain. An occupied `<remote-name>` is refused
  the way `git remote add` refuses one; `--override` repoints it instead. A
  remote already carrying that exact URL is a reported no-op, not a collision,
  so re-running is safe. `--override` writes exactly the remote it names and
  copies the replaced URL nowhere — the report echoes it (redacted) and that is
  its only record. Saving it under a second remote the caller never named was
  the previous design, and its failure mode was a name collision that reported
  a clean ✓ over a URL that had left git config for good.
  There is no URL-printing verb: `repo mirror get` already lists a clone URL per
  cluster for both forges, in a table and in `--json`.
  `remote add` and `clone` choose a placement through the shared
  `selectPlacement` picker, each passing its own `placementPicker` wording. The
  picker matches on the cluster host, which is what `--cluster` takes, and both
  verbs resolve native placements — a repo's primary and its ready mirrors —
  identically. With several placements and no `--cluster`, a terminal gets the
  picker and a script gets the **primary**, passed to `selectPlacement` by host
  rather than inferred from list order. For a native repo that is the cluster it
  lives on. For a GitHub repo it is `defaultClusterHost`, the
  cluster onboarding always places a mirror on, so it is the one every mirror
  set has in common. That is an assumption, not a lookup: a repo mirrored only
  elsewhere matches nothing and still gets the `--cluster` pointer. The server
  can name the real one (POST `/repos/resolve` returns `primaries.processing`,
  an id from the same space as `ResolvedPlacement.MirrorId`), at the cost of a
  second round trip. The picker renders on stderr when that is a terminal and on the
  controlling terminal otherwise (`openPlacementPromptTerminal`), because Bubble
  Tea fails *silently* on a redirected writer — no window size, a 0x0 viewport,
  and stdin still in raw mode. The cancellation message follows the same writer,
  so it is never explained into a stream the user is not reading.
  `edit --visibility` sets a native repo's visibility;
  `visibility get` reads it.
  **"Who can reach this repo?" is one verb, `repo grant`,** because a user
  asking it need not know which backend the repo has. `grant list` branches on
  the ref's forge before resolving anything: a `/et/` ref lists the repo's
  grants, a `/gh/` ref lists the placement's collaborators from
  `GET /mirrors/collaborators` (live GitHub-admin gated against the caller's own
  GitHub identity, so a service-account token cannot answer it). The mirror
  branch renders `GRANTEE`/`ROLE` — `grantColumns` without the provenance the
  mirror endpoint does not report — and its `--json` rewrites the
  collaborator model into the grant vocabulary through `mergeSynthesizedFields`
  — `accountId` → `granteeId`, `handle` → `granteeName`, plus `source` =
  `github`, where a mirror's access does come from — so one script reads either
  ref and no value is printed twice under two names. The merge (rather than a
  struct of our own) is what keeps any field the server adds later.
  `granteeType` stays absent — the endpoint reports an `accountId` and no kind,
  so `account` would be a guess about a principal whose kind it never gave, and
  a sentinel would add a value no server emits; absence carries the fact
  unambiguously, since the schema makes `granteeType` required on the native
  rows. `granteeName` is merged only when a name resolved, which is what the
  native rows do with theirs. That endpoint is served by the core fronting
  one cluster, so the branch resolves the repo's placements through
  `resolvePullablePlacements` (the pull-gated lookup `repo clone` and
  `remote add` share) and reads the collaborators on a cluster the repo
  actually has — preferring the default, else the first in sorted order. That
  lookup is a **hint, never a gate**: the two endpoints answer to different
  authorities, `/mirrors/placements` being pull-gated while
  `/mirrors/collaborators` runs a live GitHub-admin check, so a GitHub admin
  holding no Entire grant resolves no placement and must still be answered. A
  lookup that resolves nothing, names no dialable host, or fails outright falls
  back to `defaultClusterHost`; what the hint buys is the repo mirrored only
  outside that default. A fallback is a guess, so it never passes for a chosen
  cluster: every error names the host and the reason, and an empty read says so
  on **stderr**, which is the only channel `--json` shares — an empty array
  exits 0 and otherwise reads exactly like a mirror with no collaborators. The
  reason decides the next step, because only one of them has one that can
  answer: placements that resolved but named no dialable host point at `repo
  mirror get`, while a placement no login of yours can see points at
  `--context`, since `mirror get` reads the affiliation-scoped directory and is
  narrower than the pull-gated lookup that just came back empty. There is no region flag either: every placement
  materializes the same upstream collaborators, so the caller has nothing to
  choose. What the removed `--cluster` named was the cell — `clusterHost` is a
  required parameter of that endpoint — and the cell is now read from the
  repo's own placements. The login was never the flag's to pick and still is
  not: `runCoreForCluster` auto-selects whichever saved login the cluster
  trusts, which is what `docs/architecture/upstream-host-resolution.md`
  describes. `grant add` and
  `grant remove` keep the native path as their whole grammar: a mirror's access
  is the upstream GitHub repository's, so a `/gh/` ref is refused before any
  request — and before required flags are validated, through the target's
  `unwritableRef` PreRunE hook, so the answer is the repo rather than a missing
  `--role` — with a message naming `grant list`, rather than accepted into the
  grammar and always failing. A `github.com` URL is claimed by that refusal too,
  though it is not an accepted spelling: it names the repository unambiguously,
  so answering it with the native path's grammar would send the reader to
  rewrite the ref as the one shape that repository can never have. The branch is one optional `listBranch` field on
  `grantTarget`, so the shared builder stays forge-unaware for `org` and
  `project`.
  **A repository is named `/<forge>/<a>/<b>` and no other way**, across the
  mirror subtree and `clone` alike: a bare `<a>/<b>` is refused because both
  forges take that shape, and a GitHub URL is refused because it would be a
  second spelling for one repo. Both are recognised only to name the ref they
  should have been. One parser reads both grammars, `parseMirrorRepoRef`, and
  it takes the forges the *calling verb* serves — the same contract
  `bareRefSuggestions` uses, so a verb can never suggest a ref it refuses on the
  next line. A ref naming a forge the verb does not serve is refused **without
  being parsed**: declaring the forge is the whole answer, and quoting a name
  rule would send the reader to fix something that would be refused again.
  No verb narrows those forges today: the mirror subtree's own verbs read both
  and branch on what they get, and `repo grant list` serves both as well, so
  `unsupportedForgeErr` is reached only by the tests that pin the refusal — the
  parser keeps the narrowing because it is its contract, not because a caller
  exercises it. `repo mirror get` takes a mirror ULID or an
  `entire://` clone URL besides, since those address a placement rather than
  name a repo.
  `clone`
  accepts a native `/et/<project>/<repo>` ref, a mirror `/gh/<owner>/<repo>`
  ref, or a full `entire://` URL passed through verbatim. **Every ref names its
  forge**: the leading token alone decides which grammar is tried, and the bare
  `<project>/<repo>` shorthand was removed because both forges take that shape
  and nothing in it says which was meant (#2252).
  Requiring the prefix is a **namesquatting** guard, not tidiness: without it,
  whichever namespace the CLI defaulted to could shadow the other, and
  `TestCloneRefAlwaysRequiresItsForgePrefix` pins that no forge-less pair
  resolves in either parser, in `repo clone`, or in `resolveRepoRef` — the last
  being the surface every other repo-ref command shares. It holds only for
  *intent* —
  lookups are already unambiguous because native rows are stored prefixed in the
  same `full_name` index (`et/<project>/<repo>`), which is why the bare-pair
  `--repo` filters on `search`/`experts`/`explain` cannot cross namespaces
  either.
  The native `/et/<project>/<repo>` path is **not** clone-only: it is the
  `path` the API returns, and `resolveRepoRef` accepts it for every command
  that takes a repo ref — `view`, `edit`, `delete`, and the `visibility` and
  `protection` subtrees (COR-1632). `repo grant` takes that path and nothing else — no
  `--project`, no bare name, no ULID (its `list` reads a `/gh/` mirror ref from
  the mirror endpoint instead, never through this resolver) — through
  `resolveRepoPath`, which parses
  with `parseNativeCloneRef` and resolves both segments by name only (a project
  or repo can be *named* like a ULID, so path segments never touch the
  `looksLikeULID` passthrough). The other two clone shapes are not: a `/gh/`
  mirror ref is refused there (a mirror is in no project, so it is addressed by
  ULID), and an `entire://` URL is not parsed at all.
  Every `<project>/<repo>` name pair resolves through **one** call,
  `POST /repos/resolve` (`resolveNativeRepoByPath`), because that route needs
  `repo#pull` alone. The project-scoped routes (`GET /projects?name=`,
  `GET /projects/{id}/repos?name=`) need `project#inspect`, which a direct
  repo grant does not confer, so a repo shared with one person must never
  resolve through them. Only a `--project` **ULID** with a bare name takes the
  project-scoped listing, since there is no ULID→name route. Alongside the
  path form, `--project` is checked for agreement: a name compares
  case-insensitively before any request, a ULID against the resolved repo's
  owning project at the cost of one `GetRepo`.
  Native names are validated client-side against the server's own rules
  (`nativeProjectRe`/`nativeRepoRe`, mirroring `normalizeName` in entiredb
  `core/resource/project_name.go`); those bounds are server parity only and buy
  a local error instead of a control-plane round trip, so a failure is phrased
  as what the server accepts rather than as a rule of ours — drift is
  one-directional and only a *looser* server would make us wrong. A ref matching
  no grammar gets a targeted error (`invalidCloneRefError`) in descending
  confidence: a ref naming `github.com` is pointed at its `/gh/` form, a ref
  that declared a forge token keeps its own parser's reason, a bare pair is
  offered the forge-qualified readings that would actually parse
  (`bareRefSuggestions`), and anything left lists the accepted shapes.
  A native ref resolves name → repo ULID → `GetRepo`, whose response is the
  only one carrying both `clusterHost` and `path`, then picks among the repo's
  readable placements — the home cluster plus ready native mirrors
  (`nativePlacements`, joining mirror slugs against the cluster catalog) —
  through the same `selectPlacement`/`--cluster` flow as `/gh/` refs, and
  clones `entire://<chosen host><path>` (a native mirror serves the same
  public path as its data primary). The mirror listing and the cluster catalog
  are best-effort without `--cluster`: if either fails (a core that 404s or
  503s the listing, a catalog hiccup), resolution degrades to the home cluster
  instead of failing a clone that has always worked.
  A trailing `.git` is decoration on a `/gh/` mirror ref and **part of the name**
  on a native `/et/` one (`mirrorGitDirSuffix` documents the mechanics). GitHub
  rejects a repository name ending in `.git` outright, so on a mirror path the
  suffix can only ever be decoration and dropping it is what lets a pasted
  `git clone` URL resolve. The server is the opposite case: entiredb permits an
  interior dot (the same rule that makes `entire-trails.el` legal), `POST
  /api/v1/repos {"name":"foo.git"}` returns 201, and the data plane resolves
  `/et/` paths verbatim — so `parseNativeCloneRef` keeps the suffix, `repo
  create` forwards such a name, and a native lookup asks for `foo.git`. Trimming
  it there resolved a *different* repository — sibling repos `foo` and `foo.git`
  both exist, so `repo delete /et/p/foo.git` destroyed the neighbour and printed
  the path the user typed beside the survivor's ULID, reading as success. Every
  remaining trim is therefore a `/gh/` grammar, and
  `gitremote.splitOwnerRepo` skips the trim for `ForgeNative` so a native remote
  reads back the name it was cloned under. Two consequences worth knowing — a
  trim can *manufacture* a dot-only segment (`..git` → `.`), which both the
  `/gh/` grammar and `splitOwnerRepo` refuse; and `resolveRepoRef`'s
  project-scoped miss names the suffix in its hint rather than stripping it,
  since a bare name in that position is a name, not a path.
- The three `grant` subtrees (`org grant`, `project grant`, `repo grant`) are one
  generic builder plus three target descriptions in `grant.go`; a new target is
  a `grantTarget` value, not a fourth copy of the leaves.

  **`add` and `remove` take the grantee optionally**: omitted on a terminal,
  they open a multi-select (`grant_picker.go`). `add` then collects a role **per
  grantee**, so one run can add a reader and an admin; `--role` fixes every row,
  rendered as a non-focusable `huh` note so the pairing is shown but not
  editable. The forms sit behind the `grantPicker`, `removePicker` and
  `revokeConfirmed` seams, because `go test` has no terminal to answer them on.

  **The two pools are mirror images, and the axis is `source`.** `add` offers
  the owning org's members with **no direct grant** on the target; `remove`
  offers the **direct** grants revoking would remove. A `project:<name>` row is
  neither: project access reaches the project's repos (`push = writer +
  project->write`), but it is not a grant on the repo, so it does not block
  adding one and cannot be revoked there — revoking it really does answer "no
  such grant; nothing to revoke". `directHolders` is the one place `source` is
  read for this. `remove` also drops the `owner` row, the owning org itself,
  which holds the target through the authz schema rather than a grant.

  Both earlier versions of the add pool were wrong in opposite directions, so do
  not "simplify" back to either. Subtracting every holder emptied the pool on any
  repo whose project already covered the org, and subtracting none meant every
  row was a possible silent role change — `grant add` **upserts**, which is why
  a direct holder has nothing to add and why changing a role is the typed form's
  job.

  **`org grant add` has no picker** (`candidates` is nil, which also keeps it at
  two required args): everyone eligible is by definition absent from the only
  list there is. `org grant remove` does have one, because the members to remove
  ARE that list. There is no user search or global account listing in the API —
  the only enumerable people endpoints are `/orgs/{id}/members`,
  `/projects/{id}/members` and `/repos/{id}/grants`, none filterable — so the
  filtering is client-side. Org membership is also the only pool whose entries
  are directly grantable: project and repo rows carry a grantee ULID and no
  provider identity, with no reverse lookup, while a `Membership` carries the
  `provider:handle` that `resolveGranteeProvider` already takes. `remove` uses
  the ULID where a typed-id route exists, since it needs no lookup and survives
  a rename, so a candidate carries a `ref` to act on and a `label` to show, plus
  `byID` saying which route it takes. **A grantee is a provider-qualified handle
  and nothing else** (`ensureGranteeIsHandle`, checked before the target is
  resolved so a grantee that cannot work costs no lookup): a ULID is an internal
  id the interface does not ask anyone to copy, and routing on `byID` rather
  than on the ref's shape is what keeps the typed-id route reachable only by the
  picker, which reads the id off a listing.

  **`--role` is not a cobra-required flag**, because cobra enforces those before
  `RunE` and a role that cannot reach `RunE` cannot be prompted for. The
  guarantee it gave — an omitted role never reaching validation, a lookup, or
  the API — moved into the `RunE`, which settles both non-interactive refusals
  from the command line **before any request**: an unanswerable prompt must not
  cost a lookup.

  **An empty pool exits 0; a missing one does not.** Having nobody to add is not
  a failure — nothing went wrong and, in the common case, the state the user
  wanted already holds, the same reasoning that makes revoking an
  already-revoked grant a success rather than a 404. Selecting nobody in the
  picker is the same and also exits 0. What stays an error is a command that
  cannot run as asked: an account-owned target, which has no membership list
  anywhere, and a non-interactive run with no grantee. Those two spell out the
  `provider:handle` form because the user is stuck without it; the empty-pool
  messages do not, there being nobody left to add. Under `--json` the reason
  moves to stderr and stdout gets the empty array, so a caller parsing stdout is
  never handed a sentence.

  **Only the picker confirms a `remove`, and there is no flag to bypass it.** A
  typed grantee already names exactly who to revoke and is the form a script
  uses, so `<noun> grant remove <ref> github:alice` revokes unprompted whether or
  not a terminal is attached — gating it on terminal *detection* instead made a
  redirected stdin reach a form, read EOF, take the default of "no", and exit 0
  having revoked nothing. What a confirmation is for is the set clicked off a
  list. Gating only that path is also what leaves nothing to bypass: no caller
  who would want `--force` ever meets the prompt. Do not "finish the job" by
  adding a refusal and the flag; that is `delete`'s bargain, and the difference
  is blast radius — a deleted repo is gone, a revoked grant is one command from
  being restored. The prompt comes after the picker so it names what was chosen,
  and covers the whole set at once: a count in the title with the grantees listed
  under it, never a bare number.

  No candidate is auto-picked even when only one is eligible, unlike
  `selectPlacement`, which returns a lone cluster without prompting: this writes
  access.

  **Prompts go where the user can see them**, via `runPromptForm`: stderr when
  that is a terminal, and otherwise the controlling terminal for input *and*
  output, with the cancellation message following the prompt to the same writer.
  Pinning to stdout is wrong because `huh` writes there in accessible mode and
  these commands can be asked for `--json`; pinning to stderr is wrong because
  `... 2>log` then renders an invisible prompt on an apparently hung command.
  `selectPlacement` uses the same helper.

  **The prompt moves; the result does not.** Anything that asks a question or
  qualifies what the question is showing — the form, its cancellation line, the
  truncated-pool caveat — follows the prompt to whatever writer it rendered on.
  Anything that reports what the command DID (`✓ Revoked …`, `✓ Granted …`)
  stays on `cmd.OutOrStdout()`, because it is the command's output and a caller
  redirected it deliberately. A `--json` read piped into a parser is the case
  that settles this: the payload has to stay on stdout, so anything the command
  asks or explains goes to the terminal instead. Do not "fix" results onto the
  terminal.

  **A cancelled context is an interruption, not an answer.** A confirmation
  gate's `(false, nil)` means the user declined, and the caller exits 0 on it,
  so nothing else may borrow that pair: a context cancelled out from under the
  gate comes back as an error wrapping `ctx.Err()`. `main.go` matches
  `errors.Is(err, context.Canceled)` against the signal it recorded and
  re-raises it, which is what gives Ctrl+C its usual quiet 130 (143 for
  SIGTERM) and breaks an enclosing shell loop — swallowing the cancellation
  throws that away and exits 0 having done nothing, silently. Check either side
  of the form, as `plugin_confirm.go` does: `huh` opens the TTY during startup
  regardless of context state, and the far-side check must come before the form
  error is inspected, because `handleFormCancellation` treats `context.Canceled`
  as a clean abort. An abort from *inside* the form (`huh.ErrUserAborted`) is
  the opposite case — that one IS an answer. `confirmControlPlaneDeletion` is
  the outlier here and carries the same bug for `delete`.

  A caveat that belongs to a form goes IN the form — for the pool note, in the
  field's `Title`. `huh`'s accessible mode renders a field's title and options
  and nothing else, so a `Description` is silently dropped for exactly the
  readers who cannot see the styled version.

  **A pool is bounded; a filter is not.** A listing that IS a pool
  (`boundedList` at `coreListFetchBudget`) is read in full before a single row
  can be shown, so an unbounded walk would cost one round trip per page on a
  large org — and a truncated pool is disclosed on stderr by `reportPartialPool`
  along with the way past it. The grant rows *subtracted* from the add pool stay
  unbounded: a partial filter would offer someone the target they already hold.
  The grantee multi-select is `Filterable`, the pool being a whole org's
  membership.

  **A bounded walk cannot make a statement about the whole thing.** `listWindow`
  carries how many rows were READ (never how many survived filtering, which was
  never "the first" anything) and whether more remain, and the empty-pool
  branches consult it *before* they speak. "Every member of the org already has
  a grant on it" and "has no grants that can be revoked here" are claims about
  an org and a target; on a truncated walk neither is known, so both become an
  error naming the explicit `provider:handle` form instead — the one empty pool
  that is not a clean success, because the state the user wanted was never
  looked for past the budget.

  **The least-privileged role is named, not indexed.** `grantTarget.leastRole`
  feeds `grantPickerTarget.least`, which is what an unanswered role row starts on
  and what a refusal's example suggests. Help order runs in opposite directions —
  reader/writer/admin is least-first, owner/admin/member last — so `roles[0]` is
  right for two targets and backwards for the third.

Forge tokens (`gh`, `et`) are the path segments of an `entire://` URL, and
`gitremote.pathForges` owns the *set* — `IsForgePathToken` answers "is this a
forge token", `ForgePathLabels` gives the placeholder spelling of the segments
after it (`<owner>/<repo>` vs `<project>/<repo>`) so messages read correctly for
both. The token strings are still spelled in a dozen call sites; only the set
lives in one place. It is deliberately **not** `hostToForge`, which maps an
*upstream* git host to its id: a native repo has no upstream host, so `et` is
absent there, and conflating the two made `entire://et/<project>/<repo>` dial a
cluster named `et` while `entire://gh/...` got an actionable message.
`CanonicalHost` still reads `forgeToHost`, so a native remote falls back to its
cluster host rather than inventing a forge host. The legacy `/git/` prefix is
excluded because `repo clone` cannot act on such a ref.

**Being a forge token says nothing about which APIs accept it** — the name says
syntax on purpose. Trails are the current example: entire-api takes `et` in the
path but cannot resolve it, so `entire trail` refuses it locally with the real
reason (`errTrailsNativeUnsupported`). That refusal has to cover *both* ways a
forge reaches the API — named in `--repo` and inferred from the origin remote —
and the inferred one is the common path.

Experimental commands (gated by the build-time visibility flag above — visible
and grouped under "Experimental commands:" in developer/nightly builds, hidden
in stable releases, always runnable): `tokens`, `import`, `review`,
`blame`, `why`, `experts`, and `runner`.
`tokens` is also advertised through `entire labs`.

Top-level lifecycle and standalone commands: `enable`, `disable`, `status`,
`login`, `logout`, `clean`, `version`, `dispatch`, `activity`, `help`,
`configure`, `agent-help`, `api`, `search`. `search` is the canonical
spelling (visible in every build, grouped with Sessions & Checkpoints);
`checkpoint search` stays a working alias of the same command.

`api` is an authenticated passthrough to Entire's HTTP APIs (gh-style): it
attaches the right bearer and dials the right host so callers don't plumb auth
themselves. `--to core` (default) hits the control plane; `--to cell` hits an
entire-api cell. `--jurisdiction <slug>` (e.g. `us`, `eu`) targets a specific
jurisdiction's cell instead of the caller's home cell and implies `--to cell`
(cell routing + identity-token exchange live in `auth.NewEntireAPICellClient`
via `auth.CellTarget`). **The cell path acts as the same login `--to core`
does** — `ENTIRE_TOKEN` when set, else the selected context: with no
`ENTIRE_API_BASE_URL`, the cell `apiUrl` is read from the cluster catalog of
that login's core, so a staging login lands on a staging cell and a local-dev
login on the cell its local core advertises (never on the core itself); only an
explicit `ENTIRE_API_BASE_URL` switches to discovering a login against that
named data host (`auth.resolveCellClientSubject` has the history, COR-1634). A
`-j` slug the environment has no cell for fails naming the core consulted and
the jurisdictions it does serve. **The data API follows the acting login the
same way** (`auth.ResolveDataAPI`): `ENTIRE_TOKEN` when set (verbatim, to its
`aud`'s site), else the selected login — its JWT as the bearer and its login
server's site as the host (`us.auth.partial.to` → `https://partial.to`; a
loopback dev core has no site and needs `ENTIRE_API_BASE_URL`), so `entire
enable`, `search`, `dispatch`, `recap` and the printed trail links all land in
the login's own environment; only `ENTIRE_API_BASE_URL` switches to discovering
a saved login against a named host (an env token is sent to it verbatim), and
even then the sole eligible saved login is *named*, never used unasked —
auto-selection is a cluster rule (git remotes and the cluster-addressed
`repo mirror` commands, where the cluster pins the host), which is why
`clusterdiscovery.loginTargets.autoSelect` is set only by
`ResolveContextForCluster`. Whenever several logins are saved, every command
that acts as one says which on stderr (`Using context 'x'.`,
`auth.AnnounceContext` via `auth.ActingContext`, once per process;
`git-remote-entire` keeps its own auto-select notice) — but not when the user
named the identity with `--context`/`$ENTIRE_CONTEXT`. Resolve with
`auth.ActiveContext` instead when the login is only being described rather than
acted as, and call `auth.SilenceContextNotice` when a command must stay quiet
for its whole run (`entire agent-help` does). `activity`/`recap` fall back from
the cell to the data API freely, since both apply that precedence.
`{owner}`/`{repo}`/`{repo_id}` in the path are filled
from the current repo's origin remote. It is an escape hatch, so it is absent
from `agent-help`'s curated listing but stays in `entire help` and agent-help's
footer — an agent that needs raw access must find it rather than hand-roll curl
with a token.

`agent-help` renders machine-readable, agent-facing usage live from the Cobra
command tree (so it always matches the installed binary): bare prints a curated
"when to use entire" map; `agent-help <command>` drills into one command's
current flags; `--json` emits structured output. It is the single source of
truth the first-turn context injection and the `--agent-help-skill` skill point
agents at, instead of enumerating a surface that goes stale.

#### Where agent-facing text goes

| What you have | Where it goes |
| --- | --- |
| A new command | `agentHelpClassification` in `agent_help_cmd.go` — one entry, keyed by command path, carrying `audience` and `listed` |
| "When to use this at all" advice for agents | `agentHelpGuidance` — **never** cobra `Short`/`Long` |
| A fact humans need too (e.g. "this output is not stable") | cobra `Long`. Human help is a reference, not a lecture: whoever typed `--help` already chose the command |
| A per-task command recommendation | `agent-help`, which is pulled on demand. **Never** the first-turn injection, which carries only invariants true on every turn |

**Flag it; don't decide it.** Whether a command is `listed`, and whether it is
read-only / task-driven / user-owned, are product judgment calls — they change
what agents do unprompted in every user's repo. Take the safe default
(unlisted, user-owned), then say in the PR what you picked and why so a human
can move it. Never quietly promote a command into the listing or into
read-only.

CI enforces the mechanical parts, so trust these rather than re-deriving them:
every advertised top-level command and every child of a listed group is
classified; a read-only group contains no writing subcommand; guidance text
never appears in a command's `Short`/`Long`.

Hidden commands opt into being advertised here by setting
`Annotations[agentHelpAnnotation] = "true"` (e.g. `trail`). Because `agent-help`
renders live and lists non-hidden commands, the experimental commands appear in
`agent-help` in developer/nightly builds and are absent in stable releases — the
advertised surface is build-dependent, matching what `entire help` shows.
No-channel agents (Cursor, Copilot CLI, Factory Droid, MCP hosts — no
context-injection channel and no agent-help skill template) reach it without an
active push. All of them can discover it passively: it is visible in `entire
help`, the `entire status` footer points at it, and `entire status --json`
exposes it as the `agent_help` field. On top of that, Factory AI Droid (which is
banner-only) gets the pointer appended to its SessionStart hook banner, and
MCP-host agents can launch the hidden `entire mcp` stdio server, which exposes
`agent_help` and `entire_status` as MCP tools using the same live rendering.
Enabling a no-channel agent with `--agent-help-skill` reports the skill
unsupported and points the agent at this passive path instead.

Cobra-native aliases (no hint): `sessions` → `session`, `cp`/`checkpoints` →
`checkpoint`.

Hidden infrastructure commands: `hooks`, `trail`,
`curl-bash-post-install`, `__send_analytics`, `__sweep_sessions`, `mcp` (MCP
stdio server for MCP-host agents).

Diagnostic subcommands live alongside `doctor.go` as `doctor_logs.go` and
`doctor_bundle.go`. Group roots and noun-group children live in files
named `<noun>_group.go` and `<noun>_<verb>.go` respectively.

### Error Handling

The CLI uses a specific pattern for error output to avoid duplication between Cobra and main.go.

**How it works:**

- `root.go` sets `SilenceErrors: true` globally - Cobra never prints errors
- `main.go` prints errors to stderr, unless the error is a `SilentError`
- Commands return `NewSilentError(err)` when they've already printed a custom message

**When to use `SilentError`:**
Use `NewSilentError()` when you want to print a custom, user-friendly error message instead of the raw error:

```go
// In a command's RunE function:
if _, err := paths.WorktreeRoot(); err != nil {
    cmd.SilenceUsage = true  // Don't show usage for prerequisite errors
    fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run 'entire enable' from within a git repository.")
    return NewSilentError(errors.New("not a git repository"))
}
```

**When NOT to use `SilentError`:**
For normal errors where the default error message is sufficient, return the error directly. main.go will print it:

```go
// Normal error - main.go will print "unknown strategy: foo"
return fmt.Errorf("unknown strategy: %s", name)
```

**Key files:**

- `errors.go` - Defines `SilentError` type and `NewSilentError()` constructor
- `root.go` - Sets `SilenceErrors: true` on root command
- `main.go` - Checks for `SilentError` before printing

### `entire review` Command

`entire review` runs a configured review profile. Keep documentation brief and user-facing.

See [Review Command](../architecture/review-command.md) for usage, minimal profile config, and key files.

### Agent-Safe CLI Fallbacks

When building CLI features, do not make useful output available only through a
TUI, picker, wizard, terminal selection menu, confirmation dialog, or stdin
question. Agents must be able to complete the same read-only workflow from a
non-interactive terminal.

Plain text output is acceptable when it contains the full information needed for
the workflow. JSON is preferred for structured data, following existing patterns
such as `--json` on `status`, `agent-help`, `sessions`, `search`, and trail
finding commands. Long human-readable output may use a pager in TTY mode, but
must provide a bypass like the existing `--no-pager` pattern on `explain`.

For interactive browsing flows, provide one of these non-interactive shapes:

- a list command that prints stable identifiers, plus a show/detail command that
  accepts an identifier
- a flag or positional argument that selects the item directly
- a complete text or JSON fallback when stdout is not a terminal, like existing
  static/text fallbacks for TUI-backed commands

When reviewing CLI changes, inspect terminal-gated paths such as
`IsTerminalWriter`, `CanPromptInteractively`, Bubble Tea, `huh`, direct stdin
reads, terminal selection menus, confirmation dialogs, and wizard flows. Flag
the change if a non-interactive agent can only see a menu, preview, truncated
summary, or cannot select the item whose details matter.

Tests for interactive CLI features should cover the non-interactive path. See
[Spawning subprocesses in tests](testing.md#spawning-subprocesses-in-tests-tty-detection)
for the `execx.NonInteractive` pattern when testing a real `entire` command.

Existing good patterns:

- `entire review --findings`-style listings print a complete plain-text list and
  include a `view: ...` hint naming the detail command.
- `entire repo clone /gh/...` prompts only when several clusters are possible;
  without a TTY it asks for `--cluster`.
- `entire experts --tui` is safe because the TUI is opt-in and non-TTY output
  falls back to deterministic plain text.
- `entire explain --no-pager` is the local pattern for avoiding pager-only long
  text output.
- `entire status --json`, `entire agent-help --json`, `entire sessions list --json`,
  and trail finding commands show the local `--json` convention.

Do not require JSON everywhere. Human-readable text is fine if it contains the
complete information an agent needs. The failure mode is requiring an
interactive terminal to select something or reveal details.

## Accessibility

The CLI supports an accessibility mode for users who rely on screen readers. This mode uses simpler text prompts instead of interactive TUI elements.

### Environment Variable

- `ACCESSIBLE=1` (or any non-empty value) enables accessibility mode
- Users can set this in their shell profile (`.bashrc`, `.zshrc`) for persistent use

### Implementation Guidelines

When adding new interactive forms or prompts using `huh`:

**In the `cli` package:**
Use `NewAccessibleForm()` instead of `huh.NewForm()`:

```go
// Good - respects ACCESSIBLE env var
form := NewAccessibleForm(
    huh.NewGroup(
        huh.NewSelect[string]().
            Title("Choose an option").
            Options(...).
            Value(&choice),
    ),
)

// Bad - ignores accessibility setting
form := huh.NewForm(...)
```

**Outside the `cli` package (including `strategy`):**
Use `uiform.New(...)` from `cmd/entire/cli/uiform`. It applies the standard theme
and accessibility mode centrally; `NewAccessibleForm` is the CLI wrapper around
it. Wrap confirmations and other fields in a form, and do not duplicate the
`IsAccessibleMode` / `WithAccessible` wiring at call sites.

### Key Points

- Always use the accessibility helpers for any `huh` forms/prompts
- Test new interactive features with `ACCESSIBLE=1` to ensure they work
- The accessible mode is documented in `--help` output
