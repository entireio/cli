# Upstream Core API fixes

This client carries workarounds for bugs/gaps in the control-plane
OpenAPI document and its surface. Each item below should be fixed at the
source (the control-plane service's spec generation / route design);
doing so lets us delete the corresponding workaround here and regenerate
a cleaner client. This file is the running checklist.

When an item is fixed upstream, remove its workaround (cited by file) and
delete the entry.

## 1. Operations enumerate every error status with no shared `default`

**Symptom:** each operation declares its real success code (good — 201
for creates, 200 for reads, 204 for deletes) but then lists every error
status separately (`400`, `401`, `403`, `422`, `500`, …) with no
`default` response. ogen turns that into a per-operation sum-type result,
forcing a type switch at every call site instead of the ergonomic
`(*T, error)`.

**Fix upstream:** emit a single `default` error response (every error
already references the same `ErrorModel`, so a `default` is lossless).
ogen then generates "convenient errors" — `(*T, error)` with non-2xx as a
typed `*ErrorModelStatusCode` — straight from the spec.

**Workaround:** `spec/normalize.go` (`foldErrorResponses`) folds each
operation's explicit 4xx/5xx into one `default`, keeping the real success
code untouched. This is the one transform that is a deliberate
ergonomics choice rather than a pure bug workaround; a shared `default`
upstream retires it.

## 2. Display-only read enums hard-fail on unknown values

**Symptom:** read-model string fields the CLI only displays (`Repo.state`,
`Repo.visibility`, `Repo.objectFormat`) are declared as `enum`. ogen turns
each into a named type with a strict `Validate()` that the response decoder
calls unconditionally, so the day the server adds a new value (a new repo
lifecycle state, say) the whole `repo list` / repo-get request fails to
decode — even though the client never branches on the value.

**Fix upstream:** model client-display fields that may grow new values as
plain strings (drop `enum`), or have ogen treat them as open enums. Enums
the *client sends* (request bodies like `SetRepoVisibilityInputBody`) should
stay strict.

**Workaround:** `spec/normalize.go` (`loosenReadModelEnums`, allowlist
`readModelEnumFields`) deletes the `enum` constraint from those response
read-model fields, so ogen emits plain strings with no `Validate()` and
unknown values pass through for display. Only response read models are
loosened; request-body enums stay strict. Locked in by
`TestListProjectRepos_UnknownEnumValuesPassThrough` in `client_test.go`.
Retire the allowlist entries as upstream loosens the corresponding fields.

## 2b. New read-model fields ship as `required`

**Symptom:** `capabilities` on `Org`, `Project` and `Repo`, `Repo.provider`,
`org`/`provider` on `RepoIndexEntry`, and `ListReposOutputBody.candidatesIncomplete`
were added as `required`. ogen's
decoder then fails the whole response when a field is absent, so a core that
predates the field, or a mixed-version roll, breaks every list, get and
repo-routing call in a client that does not depend on any of them.

**Fix upstream:** add read-model fields as optional until every deployment
sends them, then tighten.

**Workaround:** `spec/normalize.go` (`loosenReadModelRequired`, allowlist
`readModelOptionalFields`) drops the listed fields from `required`. The new
`provider` and `RepoIndexEntry.permission` enums are also in
`readModelEnumFields`, since the CLI displays them or tests for the values it
knows and treats everything else as unknown (`repo protection list` tests
`Repo.provider` for `"github"` and `"entire"` separately, with a third
rendering for anything else — an absent or unrecognized provider must not
fall into either known value's branch). Remove an entry when the CLI needs
the field to be *present* to be correct — reading it through its `Opt`
accessor with a safe default is not that.

Locked in by `TestListRepos_UnsentRequiredReadFieldsDecode` and
`TestListOrgsAndProjects_UnsentCapabilitiesDecode` in `client_test.go`.
`RepoIndexEntry` gets the dedicated test because `ListRepos` is the widest
consumer: the consolidated index is what `resolveRepoCellTarget` routes with
and what `search`, `repo mirror` and the dispatch wizard page through, so a
decode failure there takes out cell routing and search together rather than
one command.

## 3. Every operation advertises the interactive login schemes

**Symptom:** the spec lists four security alternatives on every operation
(`oauth2`, `oidc`, `bearerAuth`, `sessionAuth`). `oauth2` and `oidc`
describe how a browser or device obtains a token; a client that already
holds a bearer never drives them. ogen has no generator for `openIdConnect`
and aborts on it, so the spec cannot be consumed as published.

**Fix upstream:** advertise `oauth2`/`oidc` in `components.securitySchemes`
for documentation, but list only `bearerAuth` and `sessionAuth` as the
per-operation requirements, since those are what a request actually carries.

**Workaround:** `spec/normalize.go` (`dropInteractiveSecurity`,
`interactiveSecuritySchemes`) removes the two schemes from the components
and from every security list, so the generated `SecuritySource` keeps the
`BearerAuth` and `SessionAuth` methods the client implements.

The transform refuses to *empty* a security list that had entries. An empty
operation-level `security` means "no authentication required" in OpenAPI, so
filtering an oauth2-only operation down to nothing would silently generate a
client that stops sending the bearer to it. Nothing in today's spec has that
shape; a device-code or authorize endpoint added upstream would, and it is
the only failure mode here that would not announce itself (a requirement
mixing `oauth2` with `bearerAuth` survives whole and makes ogen abort at
generate time on the now-dangling scheme).

<!-- Resolved upstream and removed:
  - Nullable arrays (`"type": ["array","null"]`) — entiredb now emits
    non-nullable arrays (`"type": "array"`, absent ⇒ `[]`), so the
    `collapseTypeUnions` transform is gone.
  - by-mirror lookup vs mirrorId delete — entiredb ENT-741 replaced the
    two-call lookup→delete with a single delete-by-coords route
    (`DELETE /mirrors?provider&owner&repo&clusterHost`); `mirror remove`
    now calls it directly and surfaces the new 404/403/503 contract. -->
