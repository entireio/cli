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

<!-- Resolved upstream and removed:
  - Nullable arrays (`"type": ["array","null"]`) — entiredb now emits
    non-nullable arrays (`"type": "array"`, absent ⇒ `[]`), so the
    `collapseTypeUnions` transform is gone.
  - by-mirror lookup vs mirrorId delete — entiredb ENT-741 replaced the
    two-call lookup→delete with a single delete-by-coords route
    (`DELETE /mirrors?provider&owner&repo&clusterHost`); `mirror remove`
    now calls it directly and surfaces the new 404/403/503 contract. -->

