# Upstream Host & Auth-Context Resolution

How the CLI decides *which host to dial* and *which login (auth context) to
authenticate as* for every upstream call. The goal is one mental model:

> An **auth context** is a login to one **core** (the identity provider /
> login server). Every upstream call resolves to some host. That host either
> **is** a core — use the active context's core directly — or it is a
> **resource server** that advertises which cores it trusts via a
> `/.well-known` blob, so the CLI picks the context whose core is trusted and
> presents that context's token to the resource.

There is no separate "auth system" per service. There is one identity model
(`contexts.json`, keyed on `CoreURL`) and a set of resource servers that
accept a core's JWTs.

## The pieces

| Role | Service (prod / staging) | Hit by | Trusted-core discovery |
|---|---|---|---|
| **Core** — IdP **and** control-plane API, co-located | `entire-core`, per region (`us.auth.entire.io`, `eu.auth.entire.io`), fronted by the apex `auth.entire.io` | `org` / `repo` / `project`, `auth *`, `login` | none needed — the host *is* the core |
| **Resource: git cluster** | `entire-server` / `entiredb` | `git-remote-entire` (clone/push) | `/.well-known/entire-cluster.json` → `core_urls` |
| **Resource: web/data API** | `entire.io` (`partial.to`) | `activity` / `search` / `trail` / `dispatch` | none by default — the acting login's site is the host (`auth.ResolveDataAPI`); under `ENTIRE_API_BASE_URL`, `/.well-known/entire-api.json` → `trusted_issuers` (bearer = the context's login JWT) |

`contexts.json` (`$ENTIRE_CONFIG_DIR/contexts.json`, shared with entiredb's
CLIs) stores each login as `{Name, CoreURL, Handle, KeychainService}` plus a
`CurrentContext` pointer. `CoreURL` is the JWT `iss` — the core that minted the
token. `entire auth switch <ctx>` flips `CurrentContext`.

### `entire login`: the apex dispatches, a region issues

`entire login --server` defaults to the apex `https://auth.entire.io`
(`api.DefaultAuthBaseURL`). The apex is a **dispatcher, not an issuer**: it
serves `/authorize` and `/device_authorization` and redirects each to the
caller's regional core, and it serves no token endpoint, no discovery
document, and no JWKS. Only a region mints tokens, with `iss`/`aud` set to
its own host — so the CLI has to discover the region mid-login and send the
token request there:

- **Browser (authorization-code) flow.** The apex 302s the browser to the
  region, which appends the RFC 9207 `iss` parameter to the loopback
  redirect. `runBrowserLogin` reads it (`BrowserAuthFlow.Issuer()`) and
  redeems the code at that host (`UseTokenIssuer`). Posting the exchange to
  the apex would 404.
- **Device flow.** The apex 307s `POST /device_authorization` to the region
  (307 preserves the method and body). `DeviceAuthStart.ResponseOrigin`
  reports the origin that actually answered, and `runLogin` points the token
  poll at it.

Both handoffs are gated by `issMatches` in `cmd/entire/cli/login.go`, which
accepts the dialled origin itself or a **strict subdomain of it over https**
(`auth.entire.io` → `us.auth.entire.io`; never `auth.entire.io.evil.com`, a
sibling, a different port, or a plaintext downgrade). The same rule validates
the `iss` claim on the returned token, so the host that receives the
authorization code and the issuer recorded in `contexts.json` are held to one
policy.

Nothing downstream of login changes: `RecordLoginContext` keys the context and
keychain slot on the token's own `iss`, and refresh + RFC 8693 exchange target
that persisted `CoreURL`. The apex is only ever the entry point.

## Resolution per call type

### Git cluster (done — `internal/entireclient/clusterdiscovery`)

`ResolveContextForCluster(host)` fetches+caches the cluster's
`/.well-known/entire-cluster.json`, reads `core_urls`, then requires the
**active context** to be issued by one of them. There is no implicit
selection — see [Account selection](#account-selection) below. The token is
then exchanged for the cluster.

### Control plane (done — this slice)

The host *is* a core, so there is no discovery. `coreapi.New()` consults
`auth.ResolveControlPlaneTarget()`, which mirrors `auth status`:

1. **active context** → its `CoreURL`, with a **per-context refreshing**
   bearer (`auth.NewRefreshingLoginProvider`): the token manager is keyed on
   `c.CoreURL` as issuer, so store reads and refresh/STS hit the right core,
   and an expired access token is silently re-minted from the stored refresh
   token. This is what makes `entire auth switch <ctx>` actually retarget
   `org`/`repo`/`project`.
2. **else** (no active context) → an error wrapping `ErrNotLoggedIn` with the
   `entire login` hint. There is no fallback host: a control-plane command
   without a login has no identity to act as. (At login time `entire login
   --server` chooses where to authenticate, and the resulting context's
   `CoreURL` *is* that host — so local-dev setups keep working.)

Key files: `cmd/entire/cli/auth/control_plane.go` (resolver),
`cmd/entire/cli/auth/refresh.go` (per-context refreshing provider),
`internal/coreapi/client.go` (`New()` + `providerSource`).

### Web/data API (done)

`activity` / `search` / `trail` / `dispatch` / `recap` / the `enable` report
**follow the acting login** (`auth.ResolveDataAPI`), with the control plane's
precedence: `ENTIRE_TOKEN` when set (the token verbatim, its `aud`'s site as
the host), else the selected login (its refreshed JWT as the bearer, its login
server's site as the host) — `us.auth.partial.to` → `https://partial.to`,
`*.entire.io` → `https://entire.io`. A login server outside those, a loopback
dev core included (the web app runs on its own port, and a core is not a
cell), is an error naming `ENTIRE_API_BASE_URL`. Printed web links (`trail`
URLs, `experts` session links) use the same origin (`auth.DataBaseURL`). There
is no ambient production default: a staging login never has its request sent
to, or its identity swapped for, entire.io.

`ENTIRE_API_BASE_URL` is the exception and names the host explicitly.
`entire.io` is a **resource server** — it validates incoming JWTs against
trusted issuers (`ENTIRE_CORE_BASE_URL` + `ENTIRE_CORE_TRUSTED_ISSUERS`) and a
fixed audience (`ENTIRE_CORE_JWT_AUDIENCE`). It **advertises** all of this at
`/.well-known/entire-api.json`, so under an override the CLI can map the API
host back to a core/context just like a git cluster:

```json
{
  "issuer": "https://us.auth.partial.to",
  "trusted_issuers": ["https://us.auth.partial.to", "https://eu.auth.partial.to"],
  "audience": "https://partial.to",
  "jwks_uris": {"https://us.auth.partial.to": "https://us.auth.partial.to/.well-known/jwks.json"}
}
```

The CLI reads **only `trusted_issuers`** — exactly the way the git path reads a
cluster's `core_urls`. `issuer`, `audience`, and `jwks_uris` are advertised but
ignored on decode (see the audience note below).

> **The bearer is the login JWT, not an exchange.** Since COR-1095 the data
> host (gateway) and the entire-api cells accept the context's login JWT — the
> *account access token* (`scope` includes `entire:session`, `aud` = its own
> core) — directly; the gateway mints per-jurisdiction cell tokens from it
> itself. The CLI previously exchanged the login JWT (RFC 8693) for a narrower
> `entire:api-access` token with `aud` = the data host origin; cell-backed
> gateway routes can no longer serve that shape (the gateway would have to
> re-exchange it at core, which refuses a non-session subject), so the exchange
> was retired. The advertised `audience` field is therefore unused.

Because the only field the CLI consumes is the trusted-issuer list — which *is*
a set of core URLs — the data-API discovery cache is literally the git cluster's
cores cache (`ClusterCoresCache`), in a separate file (`api_discovery.json`).

Resolution under an override (`auth.ResolveDataAPIToken`):

1. Resolve the API host's trusted issuers: `api_discovery.json` when fresh, else
   a live `/.well-known/entire-api.json` fetch (TLS-authenticated — it's a trust
   root; redirects refused), cached with a 24h TTL and stale-fallback on a failed
   re-fetch. Same `resolveClusterCores` shape the git path uses.
2. Require the **selected context**: it is used when its `CoreURL` is among the
   trusted issuers, and anything else is an error naming the saved login that
   would work. So `ENTIRE_API_BASE_URL=https://partial.to entire activity` with
   a prod login selected needs `entire auth switch staging` first — the target host
   never selects the identity for you, and unlike a cluster it never
   auto-selects the sole eligible login either. `ENTIRE_TOKEN` skips this
   step: the env token is sent to the named host verbatim, as the cell path
   does.
3. Return that context's login JWT, silently re-minted from the stored refresh
   token when near expiry (`auth.RefreshedLoginToken`, keyed on `c.CoreURL`
   like the control-plane provider).
4. **No fallback**: a host that doesn't advertise discovery (404 / unreachable /
   503 / malformed) with no cache entry is an error naming the host — without
   the well-known we can't know which login servers it trusts. A *reachable*
   host whose context selection fails surfaces that error — the user must log
   in or pick one. (A transient outage with a warm cache uses the stale entry.)

Key files: `cmd/entire/cli/auth/data_api.go` (`ResolveDataAPI`,
`DataBaseURL`, `ResolveDataAPIToken`),
`cmd/entire/cli/auth/refresh.go` (`RefreshedLoginToken`),
`internal/entireclient/clusterdiscovery/api_discovery.go` (`DiscoverAPI`,
`ResolveContextForAPI`, sharing `selectLoginContext` *and* the cores cache with
the cluster path), `internal/entireclient/discovery/cluster_cores.go`
(`LoadAPICores`/`ModifyAPICores`). Seams:
`NewAuthenticatedAPIClient` (activity/trail/search-completion),
`dispatch/mode_local.go` `lookupResourceToken` (dispatch),
`search_cmd.go` `resolveSearchToken` (search).

## Account selection

One rule, everywhere a host is matched — git clusters, cluster-addressed
control-plane commands, and the data API / entire-api cell routing under an
explicit `ENTIRE_API_BASE_URL` (`auth/cell_data_api.go`'s
`resolveCellClientSubject`): **the identity is the one the user selected;
failing that, for cluster-addressed operations only, the sole saved login the
host accepts.** `/.well-known` decides which identities are *accepted*. A git
remote or a cluster-addressed control-plane command (`repo mirror add` /
`remove`, and `repo grant list` reading a mirror) auto-selects because the
cluster already pins the host, so the login can follow it; every other API follows the selected login
instead, and a host that rejects it names the login that would work.

Whenever several logins are saved, every CLI command that acts as one says
which on stderr, once per process: `Using context 'x'.` (`auth.AnnounceContext`,
reached through `auth.ActingContext`). Nothing is printed when only one login is
saved, nor when the user named the identity for this invocation with
`--context`/`$ENTIRE_CONTEXT` — echoing back what they just typed is noise, and
an explicit selection is the only identity the resolvers may act as, so the
silence cannot hide a different one. `auth.ActiveContext` is the same resolution
*without* the notice, for callers that only describe the login (a printed link,
a cache key) rather than act as it, and `auth.SilenceContextNotice` suppresses
it for a whole process — `entire agent-help` uses that, because its output is
read by an agent and the login it resolves there authenticates a background
trail-enablement probe rather than requested work. `git-remote-entire` is
outside all of this and keeps its own auto-select notice below.

Cell routing with **no** `ENTIRE_API_BASE_URL` matches no host: there is no
configured data host to match against, and the production default is not a
choice the user made, so the cell path acts as the control plane does —
`ENTIRE_TOKEN`, else the selected context — and reads the cell `apiUrl` from
that login's own core catalog (COR-1634). The data API applies the same
precedence (`auth.ResolveDataAPI`), so `activity`/`recap` fall back from the
cell to the data API without changing identity or environment.

The user's selection resolves in one place, `contexts.File.Active`, with this
precedence:

| Source | Scope | Use it for |
| --- | --- | --- |
| `--context <name>` | one command | a single cross-federation command |
| `$ENTIRE_CONTEXT` | one process/shell | git operations, hooks, a whole shell session |
| `current_context` (`entire auth switch`) | persistent, machine-wide | your normal default |

The two overrides exist because `auth switch` is the wrong tool for a one-off: it
mutates state shared by every shell, worktree, and background git hook on the
machine, so forgetting to switch back silently retargets the next `git push`. And
a flag reaches only what `entire` itself spawns (it exports the flag as
`ENTIRE_CONTEXT`, below) — a `git push` you run yourself parses no `entire`
flag, so `ENTIRE_CONTEXT=staging git push` is how that one is scoped.

An override naming no saved context is a hard error
(`contexts.UnknownContextError`), never a fall-through to `current_context`:
running as an identity other than the one asked for would succeed silently as the
wrong account. It is reported before any trust check, because "that context
doesn't exist" and "that context isn't trusted here" are different mistakes.

Every consumer resolves through `Active`, so the selection is coherent: `auth
status` reports it and `auth contexts` marks it. `logout` is the one exception:
it sweeps every stored login (`auth.StoredContexts`), revoking each on its own
login server with its own bearer, so an inherited `$ENTIRE_CONTEXT` neither
narrows it nor fails it by naming a context that is gone. An explicit
`--context` is refused there instead of ignored: it asks for one identity on a
command that ends all of them, and honouring the ambient variable the same way
would make `logout` unrunnable in a shell that exports it.

Two tiers sit underneath, in `clusterdiscovery.selectLoginContext`, and they
apply only when the identity came from `current_context` (or there is none):

- exactly one saved login is eligible, **the resource is a cluster** — a git
  remote or a cluster-addressed control-plane command such as `repo mirror`
  (`loginTargets.autoSelect`, set only by `ResolveContextForCluster`), **and the
  host is under `entire.io`, `partial.to`, or `localhost`**
  (`clusterdiscovery.autoSelectSites` — prod, staging, local dev; hardcoded, no
  setting or env override) → **use it**, and
  say so on stderr (`Using context 'foo'.`, via
  `clusterdiscovery.autoSelectNoticeW`). Someone with logins in two federations
  can clone from either without retargeting every shell on the machine, and
  acting as a login they did not choose is never silent. Stderr, never stdout:
  this resolves inside `git-remote-entire`, where stdout is the remote-helper
  protocol. Nothing is printed when the selected identity acts, nor on any
  error. For any other host — a self-hosted `git.acme.com` advertising
  `auth.acme.com` — the sole eligible login is *named*, not used: the "does not
  accept your active login … These saved logins can authenticate it" error
  below, so the user selects it with `auth switch` or `--context`. The allowlist
  gates only the choice made *for* the user, never one they made.
- several are eligible → an ambiguity error naming them, sorted
  (`clusterdiscovery.ambiguousContextError`). Picking one would make the acting
  identity depend on what else happens to be stored. The error names both
  remedies — `--context <name>` / `ENTIRE_CONTEXT=<name>` for one command,
  `entire auth switch <name>` for the machine-wide default — because a
  cluster that trusts several cores (a `us` cluster advertising both the `us`
  and `eu` cores, so a cross-jurisdiction login can reach it) makes this the
  ordinary case for anyone holding a login per jurisdiction, and switching the
  default to clone once is the wrong lever.

`--context` has to cross a process boundary whenever a built-in command spawns
git against an `entire://` remote — `repo clone` execs `git clone`, and
`resume`, `explain`, `trail create` and checkpoint-policy fetch or push —
because git runs `git-remote-entire` itself and the helper selects a login from
the saved contexts on its own. The flag is therefore exported into the CLI's own
environment as `ENTIRE_CONTEXT` the moment it is parsed
(`exportContextToChildren`, `context_flag.go`), so every process the command
spawns inherits it through the same channel `ENTIRE_CONTEXT=… git push` already
uses; that includes agents launched by `review` and `investigate`, whose hooks
and pushes act as the flag's login while they run. Before the export the helper
saw only the active context and hit the ambiguity error the flag was passed to
avoid (COR-1630). A flag naming no saved login is refused in the root pre-run
(`validateContextFlag`) so the error blames `--context`, not a variable the
user never set. External plugins (`entire <plugin>`) are dispatched before
cobra parses flags and never see `--context`; scope one with
`ENTIRE_CONTEXT=… entire <plugin>`.

An **explicit** `--context`/`$ENTIRE_CONTEXT` never falls through to either: the
user asked for that identity by name, so acting as another behind their back is
the failure the override exists to prevent.

Multiple saved logins are fully supported — `auth contexts` and `auth switch`
switch between them, and `logout` removes them all.

### The advertised issuers must be the host's own

Eligibility is decided by the host's `/.well-known` document, and the eligible
login's JWT is then handed to that host: `git-remote-entire` sends it as the
bearer (`cmd/git-remote-entire/main.go`, `resolveCreds`), and the data-API path
presents the refreshed login token the same way. Nothing else asks whether the
host is *entitled* to that token. So a hostile cluster `evil.com` that advertises
`https://foo.auth.entire.io` in `core_urls` would be handed a real entire.io
login token — through every tier above, explicit or automatic, and through
`ENTIRE_TOKEN`, whose `aud` is compared against that same list.

`clusterdiscovery.requireSameSiteIssuers` closes this: every entry in `core_urls`
(git) or `trusted_issuers` (data API) must share the host's registrable domain
(eTLD+1, via `registrableDomain` — `foo.auth.entire.io` and `git.entire.io` are
both `entire.io`; `evil.co.uk` is not `acme.co.uk`; IP literals and `localhost`
match only themselves). It runs in `resolveCachedCores` on **every entry handed
out** — fresh cache, stale fallback, and a live fetch *before* it is cached — so
one check covers all three callers and a cores entry planted in the on-disk cache
is refused on read rather than trusted for a TTL.

A mismatch is a hard error naming both sides
(`cluster evil.com advertises login server https://foo.auth.entire.io outside
evil.com; refusing`), never a silent filter: an emptied list would fall through to
the `entire login --server …` hint and send the user to log in against the host
that lied. `login_url` is outside the gate — it is display-only and never
eligible (`clusterdiscovery.Response.LoginURL`); `jurisdiction_core_url` is
carried but dialled by no caller today, so it is not gated either.

Because "not logged in" is actively misleading for a user who *is* logged in,
just to another federation, the error distinguishes what the user can do about
it. Two independent facts pick the message
(`clusterdiscovery.renderUnusableActiveContext`): whether an active context
exists, and whether any saved login is eligible.

| Identity resolved | A saved login is eligible | Message |
| --- | --- | --- |
| yes | yes | names the rejected login, lists the eligible ones (sorted), points at the switch |
| yes | no | names the rejected login, adds "no other saved login does either", then the trusted servers and `entire login --server <url>` |
| no | yes | "no active auth context for …", lists the eligible ones, points at the switch |
| no | no | the login hint, plus the trusted servers and `entire login --server <url>` |

The two no-identity rows must not use the "does not accept your active login"
phrasing — there is no active login to reject.

The two "a saved login is eligible" rows are now reached only by an explicit
override the host rejected: with no override, an eligible saved login is
auto-selected or reported as ambiguous before rendering gets a say.

"Points at the switch" also tracks the source: an identity that came from
`--context` is fixed by changing that argument, not by `entire auth switch`, which
the flag would keep overriding on the next run.

The advertised servers are named whenever no saved login fits, because they are
then the only actionable detail: bare `entire login` re-authenticates against the
default server, which for a resource in another federation reproduces the same
failure.

Key file: `internal/entireclient/clusterdiscovery/resolve.go`
(`selectLoginContext` — the single home for this policy, plus `contextEligible`,
the one eligibility predicate shared by the accept decision, the auto-selection
candidates, and the candidate list reported on failure).
