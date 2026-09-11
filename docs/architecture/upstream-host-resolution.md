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
| **Core** — IdP **and** control-plane API, co-located | `entire-core`, per region (`us.auth.entire.io`, `eu.auth.entire.io`), fronted by the apex `auth.entire.io` | `org` / `repo` / `project` / `grant`, `auth *`, `login` | none needed — the host *is* the core |
| **Resource: git cluster** | `entire-server` / `entiredb` | `git-remote-entire` (clone/push) | `/.well-known/entire-cluster.json` → `core_urls` |
| **Resource: web/data API** | `entire.io` (`partial.to`) | `activity` / `search` / `trail` / `dispatch` | `/.well-known/entire-api.json` → `trusted_issuers` (bearer = the context's login JWT) |

`contexts.json` (`$ENTIRE_CONFIG_DIR/contexts.json`, shared with entiredb's
CLIs) stores each login as `{Name, CoreURL, Handle, KeychainService}` plus a
`CurrentContext` pointer. `CoreURL` is the JWT `iss` — the core that minted the
token. `entire auth use <ctx>` flips `CurrentContext`.

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
   token. This is what makes `entire auth use <ctx>` actually retarget
   `org`/`repo`/`project`/`grant`.
2. **else** (no active context) → an error wrapping `ErrNotLoggedIn` with the
   `entire login` hint. There is no fallback host: a control-plane command
   without a login has no identity to act as. (At login time `entire login
   --server` chooses where to authenticate, and the resulting context's
   `CoreURL` *is* that host — so local-dev setups keep working.)

Key files: `cmd/entire/cli/auth/control_plane.go` (resolver),
`cmd/entire/cli/auth/refresh.go` (per-context refreshing provider),
`internal/coreapi/client.go` (`New()` + `providerSource`).

### Web/data API (done)

`activity` / `search` / `trail` / `dispatch` dial `ENTIRE_API_BASE_URL`
(default `entire.io`; staging `partial.to`). `entire.io` is a **resource
server** — it validates incoming JWTs against trusted issuers
(`ENTIRE_CORE_BASE_URL` + `ENTIRE_CORE_TRUSTED_ISSUERS`) and a fixed audience
(`ENTIRE_CORE_JWT_AUDIENCE`). It now **advertises** all of this at
`/.well-known/entire-api.json`, so the CLI can map the API host back to a
core/context just like a git cluster:

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

Resolution (`auth.ResolveDataAPIToken`):

1. Resolve the API host's trusted issuers: `api_discovery.json` when fresh, else
   a live `/.well-known/entire-api.json` fetch (TLS-authenticated — it's a trust
   root; redirects refused), cached with a 24h TTL and stale-fallback on a failed
   re-fetch. Same `resolveClusterCores` shape the git path uses.
2. Require the **active context** with the same semantics as the git path: it is
   used when its `CoreURL` is among the trusted issuers, and anything else is an
   error. So `ENTIRE_API_BASE_URL=https://partial.to entire activity` needs
   `entire auth use staging` first — the target host never selects the identity
   for you.
3. Return that context's login JWT, silently re-minted from the stored refresh
   token when near expiry (`auth.RefreshedLoginToken`, keyed on `c.CoreURL`
   like the control-plane provider).
4. **No fallback**: a host that doesn't advertise discovery (404 / unreachable /
   503 / malformed) with no cache entry is an error naming the host — without
   the well-known we can't know which login servers it trusts. A *reachable*
   host whose context selection fails surfaces that error — the user must log
   in or pick one. (A transient outage with a warm cache uses the stale entry.)

The selection rule differs from the control plane (where the active context is
*always* used because there's no host to match): here a host **is** matched, so
the active context is used only when the host trusts its core, and otherwise the
sole saved login it does trust is (see [Account selection](#account-selection)).

Key files: `cmd/entire/cli/auth/data_api.go` (`ResolveDataAPIToken`),
`cmd/entire/cli/auth/refresh.go` (`RefreshedLoginToken`),
`internal/entireclient/clusterdiscovery/api_discovery.go` (`DiscoverAPI`,
`ResolveContextForAPI`, sharing `selectLoginContext` *and* the cores cache with
the cluster path), `internal/entireclient/discovery/cluster_cores.go`
(`LoadAPICores`/`ModifyAPICores`). Seams:
`NewAuthenticatedAPIClient` (activity/trail/search-completion),
`dispatch/mode_local.go` `lookupResourceToken` (dispatch),
`search_cmd.go` `resolveSearchToken` (search).

## Account selection

One rule, everywhere a host is matched — git clusters, the data API,
cluster-addressed control-plane commands, and entire-api cell routing
(`auth/cell_data_api.go`'s `resolveStoredCellSubject`): **the identity is the one
the user selected; failing that, the only saved login the host accepts.**
`/.well-known` decides which identities are *accepted*; it picks one only when
exactly one fits.

The user's selection resolves in one place, `contexts.File.Active`, with this
precedence:

| Source | Scope | Use it for |
| --- | --- | --- |
| `--context <name>` | one command | a single cross-federation command |
| `$ENTIRE_CONTEXT` | one process/shell | git operations, hooks, a whole shell session |
| `current_context` (`entire auth use`) | persistent, machine-wide | your normal default |

The two overrides exist because `auth use` is the wrong tool for a one-off: it
mutates state shared by every shell, worktree, and background git hook on the
machine, so forgetting to switch back silently retargets the next `git push`. And
a flag alone is not enough — git invokes `git-remote-entire` itself, so
`ENTIRE_CONTEXT=staging git push` is the *only* way to scope a git operation.

An override naming no saved context is a hard error
(`contexts.UnknownContextError`), never a fall-through to `current_context`:
running as an identity other than the one asked for would succeed silently as the
wrong account. It is reported before any trust check, because "that context
doesn't exist" and "that context isn't trusted here" are different mistakes.

Every consumer resolves through `Active`, so the selection is coherent: `auth
status` reports it, `auth contexts` marks it, and `logout` revokes and deletes
*that* login. Resolving the removal target separately from the revocation target
would end one session server-side while deleting another's local credentials.

Two tiers sit underneath, in `clusterdiscovery.selectLoginContext`, and they
apply only when the identity came from `current_context` (or there is none):

- exactly one saved login is eligible **and the host is under `entire.io`,
  `partial.to`, or `localhost`** (`clusterdiscovery.autoSelectSites` — prod,
  staging, local dev; hardcoded, no setting or env override) → **use it**, and
  say so on stderr (`Using context 'foo'.`, via
  `clusterdiscovery.autoSelectNoticeW`). Someone with logins in two federations
  can clone from either without retargeting every shell on the machine, and
  acting as a login they did not choose is never silent. Stderr, never stdout:
  this resolves inside `git-remote-entire`, where stdout is the remote-helper
  protocol. Nothing is printed when the selected identity acts, nor on any
  error. For any other host — a self-hosted `git.acme.com` advertising
  `auth.acme.com` — the sole eligible login is *named*, not used: the "does not
  accept your active login … These saved logins can authenticate it" error
  below, so the user selects it with `auth use` or `--context`. The allowlist
  gates only the choice made *for* the user, never one they made.
- several are eligible → an ambiguity error naming them, sorted
  (`clusterdiscovery.ambiguousContextError`). Picking one would make the acting
  identity depend on what else happens to be stored.

An **explicit** `--context`/`$ENTIRE_CONTEXT` never falls through to either: the
user asked for that identity by name, so acting as another behind their back is
the failure the override exists to prevent.

Multiple saved logins are fully supported — `auth contexts`, `auth use`, and
`logout --all-contexts` are unchanged.

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
`--context` is fixed by changing that argument, not by `entire auth use`, which
the flag would keep overriding on the next run.

The advertised servers are named whenever no saved login fits, because they are
then the only actionable detail: bare `entire login` re-authenticates against the
default server, which for a resource in another federation reproduces the same
failure.

Key file: `internal/entireclient/clusterdiscovery/resolve.go`
(`selectLoginContext` — the single home for this policy, plus `contextEligible`,
the one eligibility predicate shared by the accept decision, the auto-selection
candidates, and the candidate list reported on failure).
