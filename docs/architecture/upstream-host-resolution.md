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
| **Resource: web/data API** | `entire.io` (`partial.to`) | `activity` / `search` / `trail` / `dispatch` | **none today** — see [Deferred](#deferred) |

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

## Deferred: the web/data API (`entire.io`)

`activity` / `search` / `trail` / `dispatch` dial `ENTIRE_API_BASE_URL`
(default `entire.io`) and statically resolve their token. `entire.io` is a
**resource server** — it validates incoming JWTs against statically-configured
trusted issuers (`ENTIRE_CORE_BASE_URL` + `ENTIRE_CORE_TRUSTED_ISSUERS`) and a
fixed audience (`ENTIRE_CORE_JWT_AUDIENCE`, e.g. `entire-web-api`) — but it
does **not advertise** any of this. So the CLI can't map an `entire.io` host
back to a core/context the way it does for a git cluster.

To close the gap (so `ENTIRE_API_BASE_URL=https://partial.to entire activity`
auto-selects the right context without also setting `ENTIRE_AUTH_BASE_URL`):

1. **Server**: `entire.io` grows a `/.well-known/entire-api.json` advertising
   its trust roots. Unlike the cluster blob (`core_urls` only), the API blob
   must also carry the **audience** the CLI exchanges for:
   ```json
   {
     "issuer": "https://us.auth.partial.to",
     "trusted_issuers": ["https://us.auth.partial.to", "https://eu.auth.partial.to"],
     "audience": "entire-web-api",
     "jwks_uri": "https://us.auth.partial.to/.well-known/jwks.json"
   }
   ```
2. **CLI**: generalize the cluster resolver into a shared "host → trusted
   issuers → pick context" path whose *source* of trusted issuers is pluggable
   (cluster.json / api.json / the core itself), then exchange the context's
   token for the advertised audience via `auth.TokenForResource`. Wire it into
   the `activity` / `search` / `trail` / `dispatch` constructors
   (`NewAuthenticatedAPIClient`, `dispatch.NewCloudClient`, `search.Search`).

`ENTIRE_API_BASE_URL` names the resource host to dial (the discovery target);
the context is then chosen from the issuers that host advertises. It does not
need to be paired with `ENTIRE_AUTH_BASE_URL` once discovery exists — that is
the whole point of the well-known.
