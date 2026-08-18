# Upstream Host & Auth-Context Resolution

How the CLI decides *which host to dial* and *which login (auth context) to
authenticate as* for every upstream call. The goal is one mental model:

> An **auth context** is a login to one **core** (the identity provider /
> login server). Every upstream call resolves to some host. That host either
> **is** a core — use the active context's core directly — or it is a
> **resource server** that advertises which cores it trusts via a
> `/.well-known` blob, so the CLI picks the context whose core is trusted and
> exchanges that context's token for the resource.

There is no separate "auth system" per service. There is one identity model
(`contexts.json`, keyed on `CoreURL`) and a set of resource servers that
accept a core's JWTs.

## The pieces

| Role | Service (prod / staging) | Hit by | Trusted-core discovery |
|---|---|---|---|
| **Core** — IdP **and** control-plane API, co-located | `entire-core`, per region (`us.auth.entire.io`, `eu.auth.entire.io`), fronted by the apex `auth.entire.io` | `org` / `repo` / `project` / `grant`, `auth *`, `login` | none needed — the host *is* the core |
| **Resource: git cluster** | `entire-server` / `entiredb` | `git-remote-entire` (clone/push) | `/.well-known/entire-cluster.json` → `core_urls` |
| **Resource: web/data API** | `entire.io` (`partial.to`) | `activity` / `search` / `trail` / `dispatch` | `/.well-known/entire-api.json` → `trusted_issuers` (audience = the host origin) |

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

> **Audience = the data host origin.** entire.io's `ENTIRE_CORE_JWT_AUDIENCE` is
> `https://entire.io` (prod) / `https://partial.to` (staging) — the data host's
> own base URI, on both environments. The token manager already defaults the RFC
> 8693 audience to the resource origin it's exchanging for, so dialing
> `https://entire.io` produces `aud = https://entire.io` with no special
> handling. The CLI therefore **derives** the audience from the host it's already
> dialing rather than reading the advertised `audience` field. (This trades away
> the "server changes audience without a CLI release" flexibility — acceptable
> because `aud == base URI` is a hard requirement on both environments.)

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
3. Exchange that context's login JWT at **its** core for the data host origin
   (`auth.NewRefreshingResourceProvider`, keyed on `c.CoreURL` like the
   control-plane provider; the token manager sets `aud` = that origin).
4. **No fallback**: a host that doesn't advertise discovery (404 / unreachable /
   503 / malformed) with no cache entry is an error naming the host — without
   the well-known we can't know which login servers it trusts. A *reachable*
   host whose context selection fails surfaces that error — the user must log
   in or pick one. (A transient outage with a warm cache uses the stale entry.)

The selection rule differs from the control plane (where the active context is
*always* used because there's no host to match): here a host **is** matched, so
the active context is used only when the host trusts its core.

Key files: `cmd/entire/cli/auth/data_api.go` (`ResolveDataAPIToken`),
`cmd/entire/cli/auth/refresh.go` (`NewRefreshingResourceProvider`),
`internal/entireclient/clusterdiscovery/api_discovery.go` (`DiscoverAPI`,
`ResolveContextForAPI`, sharing `requireActiveContext` *and* the cores cache with
the cluster path), `internal/entireclient/discovery/cluster_cores.go`
(`LoadAPICores`/`ModifyAPICores`). Seams:
`NewAuthenticatedAPIClient` (activity/trail/search-completion),
`dispatch/mode_local.go` `lookupResourceToken` (dispatch),
`search_cmd.go` `resolveSearchToken` (search).

## Account selection

One rule, everywhere a host is matched — git clusters, the data API,
cluster-addressed control-plane commands, and entire-api cell routing
(`auth/cell_data_api.go`'s `resolveStoredCellSubject`): **the identity is the one
the user selected, or the command fails.** `/.well-known` decides only whether
that identity is *accepted*, never which one is *chosen*.

Selection resolves in one place, `contexts.File.Active`, with this precedence:

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

Two implicit tiers used to sit underneath — "the sole eligible context", and an
ambiguity error when several were eligible. Both are gone. They made the acting
identity depend on what else happened to be stored, so adding a second login
could silently change which account a command ran as, and the same command could
act as different identities on two machines. For "whose credentials is this
running under?", a predictable error beats a convenient guess.

Multiple saved logins are still fully supported — `auth contexts`, `auth use`,
and `logout --all-contexts` are unchanged. What changed is that switching is
always explicit.

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
phrasing — there is no active login to reject, and a dangling `current_context`
lands there.

"Points at the switch" also tracks the source: an identity that came from
`--context` is fixed by changing that argument, not by `entire auth use`, which
the flag would keep overriding on the next run.

The advertised servers are named whenever no saved login fits, because they are
then the only actionable detail: bare `entire login` re-authenticates against the
default server, which for a resource in another federation reproduces the same
failure.

Key file: `internal/entireclient/clusterdiscovery/resolve.go`
(`requireActiveContext` — the single home for this policy, plus
`contextEligible`, the one eligibility predicate shared by the accept decision
and the candidate list).
