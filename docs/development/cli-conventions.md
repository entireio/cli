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
  access list`, `repo clone`, `repo remote use`), reduced through
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
  grantee, roles reader/writer/admin; `remove` also takes an account ULID
- `repo`: control-plane repository lifecycle — `create`, `list --project`,
  `view`, `edit`, `delete`, `clone`, plus the `mirror`, `remote`, `access`,
  `visibility`, `protection` and `grant` subtrees (`repo grant` mirrors
  `project grant`, addressing the repo by its `/et/<project>/<repo>` path
  only). Verb names follow the GitHub CLI where the job is the same (`view`,
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
  detour. `remote use` repoints the *current clone's*
  git remote at a mirror (local git config only — it creates nothing
  server-side). Interactively it picks among the repo's placements and asks
  whether to replace the remote (preserving the old URL under `--upstream`) or
  add a separate one; non-interactively it repoints `--remote` directly. It
  serves both forges: for a native repo the placements are its primary plus each
  **ready** mirror. One URL per remote either way — a placement serves pushes as
  well as fetches, so there is no split fetch/push remote to maintain.
  `remote url` is the read-only half of the same subtree: it resolves a repo to
  its `entire://` URL and prints it, changing nothing.
  `remote use`, `remote url` and `clone` all choose a placement through the shared
  `selectPlacement` picker, each passing its own `placementPicker` wording. The
  picker matches on the cluster host, which is what `--cluster` takes. That
  selection is **GitHub-only**
  in `clone` and `remote url`: a native ref there resolves the repo's primary
  and `--cluster` is refused, so the way to target a native mirror is
  `remote use --cluster <host>` or a full `entire://` URL (which both `clone`
  and `remote url` forward untouched). Teaching those two to select among native
  placements is unfinished work, not a decision. It renders on stderr when that is
  a terminal and on the controlling
  terminal otherwise (`openPlacementPromptTerminal`), because Bubble Tea fails
  *silently* on a redirected writer — no window size, a 0x0 viewport, and stdin
  still in raw mode — and `remote url` exists to have its stdout captured. The
  cancellation message follows the same writer, so it is never explained into a
  stream the user is not reading.
  `remote url` is `clone` without the clone: it resolves the same three ref
  shapes through the same `resolveRepoRemoteURL` and prints the `entire://` URL
  to stdout for `git remote add entire "$(…)"`, so the two always accept the
  same refs. It deliberately does **not** take the `resolveRepoRef` grammar the
  rest of the group shares (no ULID, no `--project`) — a URL producer matches
  its sibling `clone`, not `view`. Because it prints rather than execs, its
  `entire://` passthrough is validated (`validateEntireURLForPrinting`) where
  `clone`'s is forwarded verbatim. `access list` shows who can pull a mirror (live
  GitHub-admin gated). `edit --visibility` sets a native repo's visibility;
  `visibility get` reads it.
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
  `repo access list` is the one verb still GitHub-only (it reads GitHub
  collaborators; native access is grants), and it points a native ref at
  `entire repo grant list`. `repo mirror get` takes a mirror ULID or an
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
  `--project`, no bare name, no ULID — through `resolveRepoPath`, which parses
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
  A trailing `.git` is never part of a repo name, on **either** backend
  (`gitDirSuffix` documents the mechanics): every ref parser drops it and `repo
  create` refuses a name ending in it. This is a deliberate client-side
  narrowing — GitHub rejects such a name outright, but the server accepts a
  native `foo.git` (interior dot, same rule that makes `entire-trails.el` legal)
  and strips the suffix for `/gh/` paths only. `gitremote.splitOwnerRepo` trims
  unconditionally when reading a remote back, so such a repo is unaddressable by
  name once cloned regardless; dropping it everywhere makes the CLI agree with
  itself instead of leaving `repo clone` the one path that keeps it. Escape
  hatches: the repo's ULID, or a full `entire://` URL. Two consequences worth
  knowing — a `foo.git` created through the API or web UI *aliases* onto `foo`
  in `resolveRepoRef`, and the durable fix is a server-side rule in
  `normalizeName`, not this check.
- The three `grant` subtrees (`org grant`, `project grant`, `repo grant`) are one
  generic builder plus three target descriptions in `grant.go`; a new target is
  a `grantTarget` value, not a fourth copy of the leaves.

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
