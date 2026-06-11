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
| **Core** — IdP **and** control-plane API, co-located | `entire-core` (`us.auth.entire.io`) | `org` / `repo` / `project` / `grant`, `auth *`, `login` | none needed — the host *is* the core |
| **Resource: git cluster** | `entire-server` / `entiredb` | `git-remote-entire` (clone/push) | `/.well-known/entire-cluster.json` → `core_urls` |
| **Resource: web/data API** | `entire.io` (`partial.to`) | `activity` / `search` / `trail` / `dispatch` | `/.well-known/entire-api.json` → `trusted_issuers` (audience = the host origin) |

`contexts.json` (`$ENTIRE_CONFIG_DIR/contexts.json`, shared with entiredb's
CLIs) stores each login as `{Name, CoreURL, Handle, KeychainService}` plus a
`CurrentContext` pointer. `CoreURL` is the JWT `iss` — the core that minted the
token. `entire auth use <ctx>` flips `CurrentContext`.

## Resolution per call type

### Git cluster (done — `internal/entireclient/clusterdiscovery`)

`ResolveContextForCluster(host)` fetches+caches the cluster's
`/.well-known/entire-cluster.json`, reads `core_urls`, then selects the
context: active-context-wins if its `CoreURL` is among the cores, else the
sole eligible context, else an error (zero → login hint; ambiguous → asks for
`auth use`). The token is then exchanged for the cluster.

### Control plane (done — this slice)

The host *is* a core, so there is no discovery. `coreapi.New()` consults
`auth.ResolveControlPlaneTarget()`, which mirrors `auth status`:

1. **active context** → its `CoreURL`, with a **per-context refreshing**
   bearer (`auth.NewRefreshingLoginProvider`): the token manager is keyed on
   `c.CoreURL` as issuer, so store reads and refresh/STS hit the right core,
   and an expired access token is silently re-minted from the stored refresh
   token. This is what makes `entire auth use <ctx>` actually retarget
   `org`/`repo`/`project`/`grant`.
2. **else** (no active context) → the configured auth origin
   (`ENTIRE_AUTH_BASE_URL` or the default) + `TokenForResource` — the
   pre-contexts fallback.

`ENTIRE_AUTH_BASE_URL` is the fallback host, **not** an override: a token
minted by the active context's core can't authenticate against a different
host, so the active context always wins when present. (At login time the env
var still chooses where to authenticate, and the resulting context's `CoreURL`
*is* that host — so local-dev / split-host setups keep working.)

Key files: `cmd/entire/cli/auth/control_plane.go` (resolver),
`cmd/entire/cli/auth/refresh.go` (per-context refreshing provider),
`internal/coreapi/client.go` (`New()` + `providerSource`),
`cmd/entire/cli/api/base_url.go` (`AuthBaseURLOverridden`).

Why the per-context path and not the singleton manager: the singleton
(`auth/exchange.go:defaultManager`) is built once with `Issuer =
api.AuthBaseURL()`. When the active context lives on a *different* core, both
its token-store reads and its STS/refresh endpoint are keyed on the wrong
host. The per-context provider fixes that by keying on `c.CoreURL`.

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
2. Pick the context with the **same cluster semantics** as the git path:
   active-context-wins-if-eligible → sole eligible → explicit-choice error.
   This is the lever that makes `ENTIRE_API_BASE_URL=https://partial.to entire
   activity` authenticate as the partial.to login even while the active context
   is a prod entire.io login — without also setting `ENTIRE_AUTH_BASE_URL`.
3. Exchange that context's login JWT at **its** core for the data host origin
   (`auth.NewRefreshingResourceProvider`, keyed on `c.CoreURL` like the
   control-plane provider; the token manager sets `aud` = that origin).
4. **Fallback**: if the host doesn't advertise discovery (404 / unreachable /
   503 / malformed) *and* no cache entry exists, fall back to the pre-discovery
   static path (`TokenForResource` via the singleton manager), so behaviour is
   never worse than before. A *reachable* host whose context selection fails
   surfaces that error — the user must log in or pick one. (A transient outage
   with a warm cache uses the stale entry, not the fallback.)

The selection rule differs from the control plane (where the active context
*always* wins because there's no host to match): here a host **is** matched, so
the active context wins only when eligible.

Key files: `cmd/entire/cli/auth/data_api.go` (`ResolveDataAPIToken` + fallback),
`cmd/entire/cli/auth/refresh.go` (`NewRefreshingResourceProvider`),
`internal/entireclient/clusterdiscovery/api_discovery.go` (`DiscoverAPI`,
`ResolveContextForAPI`, sharing `selectContext` *and* the cores cache with the
cluster path), `internal/entireclient/discovery/cluster_cores.go`
(`LoadAPICores`/`ModifyAPICores`). Seams:
`NewAuthenticatedAPIClient` (activity/trail/search-completion),
`dispatch/mode_local.go` `lookupResourceToken` (dispatch),
`search_cmd.go` `resolveSearchToken` (search).
