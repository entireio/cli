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

## 3. Idempotent CI-webhook enroll returns an undocumented 200

**Symptom:** `POST /repos/{repoId}/ci-webhooks` is idempotent — it answers
`201` on a fresh enroll and `200` on a re-enroll of an existing
subscription, both carrying the same `CIWebhookView` body. But the huma
operation only sets `DefaultStatus: 201`, so the OpenAPI document lists
`201` alone. ogen's decoder then treats the `200` re-enroll as an
unexpected status and fails to decode it (it falls through to the
`application/problem+json` error branch and errors on the `application/json`
content type), surfacing an idempotent success as a client error.

**Fix upstream:** have huma advertise `200` as an additional response on the
enroll operation (or document the idempotent behavior with a both-codes
response set), so the generated client accepts the re-enroll.

**Workaround:** `spec/normalize.go` (`addCreateCIWebhookReenrollStatus`)
replaces the operation's single literal `201` with a `2XX` range (same
`CIWebhookView` schema). ogen then decodes any 2xx into one concrete
`*CIWebhookViewStatusCode` — declaring `200` and `201` as separate responses
would instead force a per-status sum type, breaking the `(*T, error)`
ergonomics the rest of the client relies on (see item 1). The wrapper's
`StatusCode` still distinguishes a fresh enroll (201) from a re-enroll (200).
Retire this transform once the enroll operation advertises the 200 upstream.

<!-- Resolved upstream and removed:
  - Nullable arrays (`"type": ["array","null"]`) — entiredb now emits
    non-nullable arrays (`"type": "array"`, absent ⇒ `[]`), so the
    `collapseTypeUnions` transform is gone.
  - by-mirror lookup vs mirrorId delete — entiredb ENT-741 replaced the
    two-call lookup→delete with a single delete-by-coords route
    (`DELETE /mirrors?provider&owner&repo&clusterHost`); `mirror remove`
    now calls it directly and surfaces the new 404/403/503 contract. -->
