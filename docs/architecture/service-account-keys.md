# Service-account keys

A machine with no workload OIDC issuer (a dev box in a cloud account, a self-hosted runner) authenticates as a service account with an Ed25519 key it generated itself. The private key never leaves the machine; the server only ever holds the public half. Every process signs a short-lived assertion with the key and exchanges it for an ordinary sa-session JWT, which then flows through every path an `ENTIRE_TOKEN` would.

**Status: CLI prototype.** The commands are experimental (`entire auth service-account`, hidden in stable builds) and the server side described here does not exist yet. This document is the proposed contract; the CLI code in `cmd/entire/cli/auth/service_account*.go` implements the client half of it. Workload OIDC federation stays the first choice wherever the platform issues tokens.

## Client behavior

### Selection and precedence

`ENTIRE_SERVICE_ACCOUNT=svc_<ulid>` opts a process in. A key on disk is never used without it, so a human login on the same machine stays the default. Precedence is `ENTIRE_TOKEN` > `ENTIRE_SERVICE_ACCOUNT` > stored contexts. Every consumer that used to read `ENTIRE_TOKEN` directly goes through `auth.LookupEnvToken` or `auth.ServiceAccountTarget`; see [API routing](../development/api-routing.md).

A configured service account **fails closed**: a blank variable, a missing key, a refused exchange or an unusable session token is an error. Nothing falls back to a stored human login, and `entire enable` refuses to derive a git identity from a service account (it prints the `git config` alternative instead).


### Storage

All files live in the per-user config directory (`userdirs.ConfigRoot`), mode 0600, and every change is **durable**: data is fsynced before it is renamed or linked into place, the temporary alias of an exclusive create is removed before the directory is fsynced, and removals of key, state, pointer and session files fsync their directory too (`jsonutil.WriteFileDurableIn`, `CreateFileDurableIn`, `RemoveDurableIn`). None of these files can be rebuilt, and a deleted credential must not come back after a power loss, so the repo's usual rename-without-fsync is not enough here.

| File | Content | Written |
| --- | --- | --- |
| `service-account-key-<key id>.json` | The key: id, server, label, creation time, PKCS#8 private key | Once, exclusively (hard link fails rather than replaces); never rewritten |
| `service-account-key-<key id>.state.json` | Its account (bound, or proposed while enrolling) and any enrollment in flight: phase, enrollment id, expiry, polling interval | Replaced atomically on each transition |
| `service-account-<svc>.active.json` | The account's active key, and the list of keys that still have to revoke themselves | Replaced atomically |
| `service-accounts.lock` | Held by every change to the three above | |

The key id is the RFC 7638 JWK thumbprint of the public key, so it names the files and the key at once. Separating the immutable key from its mutable state means no transition ever rewrites private key material. Ordering rules: a state file is written before its key file and deleted after it, so a key file never exists without its state; a key is on disk before any server learns its public half. Reading a key with group or other permission bits prints a warning, like the file token store.

The single-file format of the first prototype (`service-account-<svc>.json`) is not migrated: it never shipped outside the prototype branch. If such a file is found where an active key is expected, the error says so and asks for a new enrollment.

The session cache is `service-account-session-<key id>.json` in the per-user cache directory, recording server, account and key id; a mismatch on any of them is a miss. A per-key lock in the cache directory serialises exchanges and cache removal across processes; the verification exchange of `key activate` is never cached. A minted token is used only if its unverified claims have `sub` equal to the service account, **exactly one URL-shaped audience, equal to the key's server** (routing elsewhere takes the first URL audience, so a second origin could otherwise receive the bearer), `sa_key_id` equal to this key's id, and `exp` in the future and within 24 hours; a cached token is re-checked the same way on every read.

### Key life cycle

| Status | Meaning |
| --- | --- |
| `enrolling` | Enrollment undecided: `submitting` (the create request may or may not have reached the server) or `pending` (awaiting a decision). A key in phase `approved` (bound, bind not yet finished) shows its real status, marked for `enroll --resume`. |
| `active` | Selected by the account's pointer; the key that signs |
| `inactive` | Bound to an account but not selected: a staged successor |
| `retired` | In the pointer's retiring list: replaced, and still has to revoke itself |

Transitions are enforced in the store, not in the commands:

- **Only an explicit server answer removes an enrolling key** (`ForgetEnrollment`): `denied`, `expired`, a submission refused with a 4xx, or a cancel confirmed with 204. Elapsed time, lost responses, 5xx, timeouts and 404s **never** count: an enrollment may have been approved before its record went away. The only other way out is the user's explicit `enroll --cancel --local-only`, which warns that an approved key would stay trusted until an admin revokes it. An undecided key cannot be revoked or deleted by any other path, including `key revoke --key-id` and `--local-only`.
- **Submission is the lookup.** The key is stored in phase `submitting` before the request. The server answers by key id with what it holds, so `enroll --resume` always resubmits: it finds the live enrollment, the decision already made (`approved` binds the key; `denied`/`expired` delete it), or, if the record is gone and the key was never approved, a fresh enrollment to approve.
- **Polling** uses the server's interval (persisted for `--resume`), clamped to 1–60 s; neither a sleep nor a poll runs past the expiry. At expiry one last poll, bounded by the 30-second request timeout, catches a last-moment decision; "still pending" or a 404 then keeps the key and points at `--resume`.
- **Cancel** deletes the key only on 204, or on 409 with `denied`/`expired`. Any other success status (including 200) fails closed. On 409 `approved` the approval won the race: the key is kept and bound, and the user is told how to revoke it. On 404 the client looks the key up by resubmitting and follows the answer (cancelling a fresh pending enrollment found that way once); anything unresolved keeps the key.
- **Binding** is three recoverable writes: mark the key `approved` with its account, set the pointer if the account has no active key, clear the enrollment. `enroll --resume` selects keys by enrollment phase, not status, so it also finds a key that became active before the last write, and finishes it; binding an already-bound key is a no-op, so concurrent resumes agree.
- **Activation** first runs a real exchange with the new key (not cached) and changes nothing if the server refuses it. One atomic pointer write then selects the new key **and** moves the old one into the retiring list, so the obligation to revoke it cannot be lost. Each retiring key revokes itself and is deleted, and only then leaves the list; an entry whose files are already gone is dropped on the next activation. `key activate` with nothing left to switch to retries every pending revoke.
- **Lifecycle decisions fail closed on a damaged store.** The one-enrollment-per-machine rule, resume and cancel selection, and activation scan every key entry and stop on any unreadable or non-regular one; only `key list` reads best-effort, reporting such entries (`problems` in `--json`).

### Transport rules

Every key-signed request requires HTTPS, except loopback or an explicit `--insecure-http-auth` for that invocation. A key created for a plain-HTTP development server stays listable, showable and deletable without the flag; only network use needs it. Such a key can manage itself and back `coreapi` clients, but `ENTIRE_SERVICE_ACCOUNT` refuses it on the ENTIRE_TOKEN-compatible paths (`auth status`/`token`, data API, cells, git), whose audience check accepts only https, just as it does for `ENTIRE_TOKEN`. Key-management requests never follow redirects (a 307/308 would resend the proof elsewhere), and the STS client refuses cross-host redirects. Approval links must be HTTPS, carry no userinfo and consist of printable ASCII; user codes and enrollment ids must match fixed patterns; server error text is passed through the terminal sanitizer and capped before it is printed.

## Server contract (proposed)

### Proofs

Every key-proved call carries a compact JWS signed by the key:

- header `alg` = `EdDSA`, `typ` = `JWT`, `kid` = the key id (RFC 7638 thumbprint, recomputable from the registered public key);
- `iss` = `sub` = the service account for exchange and revoke, the key id for enrollment calls (the key has no account yet);
- `aud` = `<server>/api/authz/sts` for the exchange, otherwise **the exact endpoint URL**;
- for key-management calls, `htm` = `POST` and `bh` = base64url SHA-256 of the request body **without** its `proof` member, serialised per RFC 8785 (JCS). The client keeps every body string within printable ASCII plus the PEM's newlines (labels are sanitised to printable ASCII without `"` and `\`, at most 64 bytes), for which sorted-key JSON without HTML escaping is exactly JCS;
- `iat`, `exp` = `iat` + 5 minutes, and a random `jti`.

The server must verify the signature against the registered (or, for enrollment, submitted) public key, check `aud`, `htm` and `bh` against the request it received, reject `exp` in the past or more than 5 minutes ahead (allowing small clock skew), and **refuse a reused `jti`** for as long as the proof could be valid. A replay cache keyed by (key id, `jti`) with a TTL of the remaining lifetime suffices. With `aud`, `htm` and `bh` bound, a captured proof cannot be replayed against another endpoint or with another body.

### Endpoints

| Call | Request | Response |
| --- | --- | --- |
| Token exchange `POST /oauth/token` | RFC 8693 form: `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, `subject_token=<proof>`, `subject_token_type=urn:ietf:params:oauth:token-type:jwt`, `requested_token_type=urn:entire:token-type:sa-session`, `resource=svc_…`, `audience=<server>`; client `entire-cli` | `{access_token, token_type, expires_in}` |
| Submit enrollment `POST /api/v1/service-account-enrollments` | `{public_key (PKIX PEM), key_id, service_account?, name, proof}` | 200/201 `{status, service_account?, enrollment_id, user_code, verification_uri, verification_uri_complete?, expires_in, interval}`. **The authoritative lookup by `key_id`**: for a key ever approved and not revoked, `status: approved` with its account, for as long as the key exists; for an undecided enrollment, that enrollment (`status: pending`); for a recorded denial or expiry, that status; only for a key the server has no record of, a new enrollment. A 4xx (other than 408/425/429) means the key is not and will not become trusted, e.g. a revoked key. |
| Poll `POST /api/v1/service-account-enrollments/{id}/poll` | `{proof}` | 200 `{status: pending\|approved\|denied\|expired, service_account}`; 404 for an id the server never issued or has purged (the client then looks up by key id) |
| Cancel `POST /api/v1/service-account-enrollments/{id}/cancel` | `{proof}` | **204** cancelled (it can no longer be approved) — the only success status; **409 `{status: approved\|denied\|expired, service_account}`** when already decided; 404 for an unknown or purged id |
| Self-revoke `POST /api/v1/service-accounts/{svc}/keys/{key id}/revoke` | `{proof}` | 204; 404 when unknown or already revoked |

`expires_in` is at most one hour. Decided and expired enrollment records should be kept for at least 24 hours, but the client never relies on that: an approved key's registration is itself the lasting record that the key-id lookup answers from.

The exchange is a new branch of the existing STS: a subject token whose `iss` is a `svc_` id is verified against that account's registered, non-revoked keys. The minted sa-session JWT **must** carry `sub` = the service account, `aud` = exactly the server origin (no other URL audience; the client refuses anything else), and **`sa_key_id`** (string) = the RFC 7638 thumbprint of the key that minted it; the client refuses a session whose `sa_key_id` is missing or not its own key.

### Registration and activation

- Keys are modelled like plugin keys (`core/plugins/plugin.go` `PublicKeys`): a small capped set per service account, each with id, label, created-by, created-at and last-used-at.
- **Enrollment** creates a pending record holding the public key, key id, proposed account, label and the server-observed client IP. The submit call is unauthenticated apart from the proof of possession, so it needs per-IP rate limits and a cap on pending enrollments per server. The approval page shows the user code, key id, label and IP, and approval requires `manage` on the target project (the permission that governs service-account grants). The approver may pick or create the account.
- Approval and cancellation of one enrollment are serialised: exactly one of them wins, and the loser's response (poll or cancel 409) reports the winner.
- The key becomes usable for exchanges at approval. The client stores it before submitting, so there is no window where the server trusts a key the client lost.
- An admin path to register a public key directly (for `key create`) and to list and revoke any key of an account belongs in the service-account management surface.

### Revocation

- A key may revoke only itself; revoking other keys is an admin action. Both must be audit-logged.
- **Revocation must end live sessions immediately.** `sa_key_id` is the lineage claim: every token derived from an sa-session (repo-scoped push/pull tokens, on-behalf-of exchanges) **must** copy it unchanged, and every verifier of such tokens (core, cells, git) **must** reject a token whose `sa_key_id` names a revoked key, the way `checkPluginFamily` ends plugin tokens. Without this, a revoked key keeps working for as long as its sessions and their derived tokens live (the client caps sessions at 24 hours), so this is a requirement, not an optimisation.
- Retired keys are revoked by the client right after `key activate`; an admin-set maximum key lifetime (open question) would cover machines nobody decommissions.

### Rollout compatibility

Everything is additive. Servers without the endpoints answer 404 to enrollment (a 4xx: the CLI reports it and deletes the never-submitted key) and `invalid_grant`/`invalid_request` to an unknown subject issuer at the STS, which the CLI surfaces as a failed exchange with no fall-back. No existing token type, endpoint or claim changes meaning. The commands stay experimental until the server half ships in every region.

## Open questions

- Does the approver pick the account (and its grants) during approval, or must the machine name one? The CLI supports both.
- Should a key carry an optional permission ceiling below the account's grants, or is "one service account per purpose" the scoping tool?
- Should a key be able to register its own successor (self-rotation without an admin), and if so how is a stolen key prevented from persisting itself?
- An admin-set maximum key lifetime.
- **Atomic rotation (recommended).** `key activate` verifies the new key, switches, then revokes the old one: three steps with a window in which both keys are valid. A server operation `POST /api/v1/service-accounts/{svc}/keys/{new key id}/activate`, proved by the new key and naming the key it replaces, which revokes the old key in the same transaction, would close that window; the client would keep its local pointer-and-retiring-list recovery around it.
