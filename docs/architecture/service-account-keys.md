# Service-account keys

A machine with no workload OIDC issuer (a dev box in a cloud account, a self-hosted runner) authenticates as a service account with an Ed25519 key it generated itself. The private key never leaves the machine; the server only ever holds the public half. Every process signs a short-lived assertion with the key and exchanges it for an ordinary sa-session JWT, which then flows through every path an `ENTIRE_TOKEN` would.

**Status: CLI prototype.** The commands are experimental (`entire auth service-account`, hidden in stable builds) and the server side described here does not exist yet. This document is the proposed contract; the CLI code in `cmd/entire/cli/auth/service_account*.go` implements the client half of it. Workload OIDC federation stays the first choice wherever the platform issues tokens.

## Client behavior

### Selection and precedence

`ENTIRE_SERVICE_ACCOUNT=svc_<ulid>` opts a process in. A key on disk is never used without it, so a human login on the same machine stays the default. Precedence is `ENTIRE_TOKEN` > `ENTIRE_SERVICE_ACCOUNT` > stored contexts. Every consumer that used to read `ENTIRE_TOKEN` directly goes through `auth.LookupEnvToken` or `auth.ServiceAccountTarget`; see [API routing](../development/api-routing.md).

A configured service account **fails closed**: a blank variable, a missing key, a refused exchange or an unusable session token is an error. Nothing falls back to a stored human login, and `entire enable` refuses to derive a git identity from a service account (it prints the `git config` alternative instead).

### Storage

All files live in the per-user config directory (`userdirs.ConfigRoot`), mode 0600:

| File | Content |
| --- | --- |
| `service-account-key-<key id>.json` | One key: id, server, account (empty while an unbound enrollment is pending), label, pending enrollment id and expiry, retirement time, PKCS#8 private key |
| `service-account-<svc>.active.json` | Which key the account signs with |
| `service-accounts.lock` | Held by every change to the two above |

The key id is the RFC 7638 JWK thumbprint of the public key, so it names the file and the key at the same time. Key files are created exclusively (temp file, then hard link, which fails rather than replaces); only the key's own state fields are ever rewritten. Switching keys is one atomic rewrite of the pointer, so there is never a moment without a usable key. Reading a key with group or other permission bits prints a warning, like the file token store.

The session cache is `service-account-session-<key id>.json` in the per-user cache directory, recording server, account and key id; a mismatch on any of them is a miss. A per-key lock in the cache directory makes concurrent processes share one exchange. Before a minted token is cached or used, its unverified claims must have `sub` equal to the service account, `aud` containing the key's server, and `exp` in the future and within 24 hours; the token's `exp` wins over a later `expires_in`. Deleting a key also deletes its cached session, and a failure to do so is reported.

### Key life cycle

| Status | Meaning |
| --- | --- |
| `enrolling` | Written to disk, enrollment submitted or about to be, awaiting approval |
| `active` | Selected by the account's pointer; the key that signs |
| `inactive` | Bound to an account but not selected: a staged successor |
| `retired` | Replaced; still on disk only until it has revoked itself |

- `enroll` writes the key **before** submitting it, so the server never trusts a key whose private half does not exist. An interrupted wait keeps the key: `enroll --resume` continues polling and `enroll --cancel` withdraws the enrollment and deletes the key. A denied or expired enrollment deletes it. Only one enrollment may be pending at a time.
- An approved or created key becomes active only if the account has no active key on this machine. Otherwise it is stored inactive. The approver binding the key to a different account than proposed therefore never displaces another account's key.
- `key activate` first runs a real exchange with the new key and changes nothing if the server refuses it. It then switches the pointer, marks the previous key retired, and has the previous key revoke itself. A failed revoke keeps the retired key so `key revoke --key-id` can retry.
- `key revoke` revokes the active key, or `--key-id`, with the key's own proof, then deletes it. A failed server call keeps it; `--local-only` deletes without revoking and warns that the key stays valid.

### Transport rules

Every key-signed request requires HTTPS, except loopback or an explicit `--insecure-http-auth` for that invocation. A key created for a plain-HTTP development server stays listable, showable and deletable without the flag; only network use needs it. Key-management requests never follow redirects (a 307/308 would resend the proof elsewhere), and the STS client refuses cross-host redirects. Enrollment timing from the server is clamped (poll interval 1–60 s, lifetime up to one hour) and the wait never sleeps past the enrollment's expiry. Approval links must be HTTPS, carry no userinfo and consist of printable ASCII; user codes and enrollment ids must match fixed patterns; server error text is passed through the terminal sanitizer and capped before it is printed.

## Server contract (proposed)

### Proofs

Every key-proved call carries a compact JWS signed by the key:

- header `alg` = `EdDSA`, `typ` = `JWT`, `kid` = the key id (RFC 7638 thumbprint, recomputable from the registered public key);
- `iss` = `sub` = the service account for exchange and revoke, the key id for enrollment calls (the key has no account yet);
- `aud` = `<server>/api/authz/sts` for the exchange, otherwise **the exact endpoint URL**, so a proof captured for one call is useless for any other;
- `iat`, `exp` = `iat` + 5 minutes, and a random `jti`.

The server must verify the signature against the registered (or, for enrollment, submitted) public key, reject `exp` in the past or more than 5 minutes ahead (allowing small clock skew), and **refuse a reused `jti`** for as long as the proof could be valid. A replay cache keyed by (key id, `jti`) with a TTL of the remaining lifetime suffices.

### Endpoints

| Call | Request | Response |
| --- | --- | --- |
| Token exchange `POST /oauth/token` | RFC 8693 form: `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, `subject_token=<proof>`, `subject_token_type=urn:ietf:params:oauth:token-type:jwt`, `requested_token_type=urn:entire:token-type:sa-session`, `resource=svc_…`, `audience=<server>`; client `entire-cli` | `{access_token, token_type, expires_in}` |
| Start enrollment `POST /api/v1/service-account-enrollments` | `{public_key (PKIX PEM), key_id, service_account?, name, proof}` | 200/201 `{enrollment_id, user_code, verification_uri, verification_uri_complete?, expires_in, interval}` |
| Poll `POST /api/v1/service-account-enrollments/{id}/poll` | `{proof}` | 200 `{status: pending\|approved\|denied\|expired, service_account}`; 404 when unknown |
| Cancel `POST /api/v1/service-account-enrollments/{id}/cancel` | `{proof}` | 204; 404 when unknown or already decided |
| Self-revoke `POST /api/v1/service-accounts/{svc}/keys/{key id}/revoke` | `{proof}` | 204; 404 when unknown or already revoked |

The exchange is a new branch of the existing STS: a subject token whose `iss` is a `svc_` id is verified against that account's registered keys. The minted sa-session JWT must carry `sub` = the service account and `aud` = the server origin (the client refuses anything else), and should carry the key id (for example in `act` or a dedicated claim) so revocation can end live sessions.

### Registration and activation

- Keys are modelled like plugin keys (`core/plugins/plugin.go` `PublicKeys`): a small capped set per service account, each with id, label, created-by, created-at and last-used-at.
- **Enrollment** creates a pending record holding the public key, key id, proposed account, label and the server-observed client IP. The create call is necessarily unauthenticated apart from the proof of possession, so it needs per-IP rate limits and a cap on pending enrollments per server. The approval page shows the user code, key id, label and IP, and approval requires `manage` on the target project (the permission that governs service-account grants). The approver may pick or create the account.
- The poll for an approved enrollment keeps answering `approved` for the enrollment's whole lifetime, so a client that lost the response can re-poll (`enroll --resume`). The key becomes usable for exchanges at approval; the client stores it before submitting, so there is no window where the server trusts a key the client lost.
- An admin path to register a public key directly (for `key create`) and to list and revoke any key of an account belongs in the service-account management surface.

### Revocation

- A key may revoke only itself; revoking other keys is an admin action. Both must be audit-logged.
- Revocation should end live sessions immediately, as `checkPluginFamily` does for plugin tokens: sessions carry the key id and the token check rejects a revoked key's sessions. Without that, a revoked key's sessions live until they expire.
- Retired keys are revoked by the client right after `key activate`; an admin-set maximum key lifetime (open question) would cover machines nobody decommissions.

### Rollout compatibility

Everything is additive. Servers without the endpoints answer 404 to enrollment (the CLI reports it) and `invalid_grant`/`invalid_request` to an unknown subject issuer at the STS, which the CLI surfaces as a failed exchange with no fall-back. No existing token type, endpoint or claim changes meaning. The commands stay experimental until the server half ships in every region.

## Open questions

- Does the approver pick the account (and its grants) during approval, or must the machine name one? The CLI supports both.
- Should a key carry an optional permission ceiling below the account's grants, or is "one service account per purpose" the scoping tool?
- Should a key be able to register its own successor (self-rotation without an admin), and if so how is a stolen key prevented from persisting itself?
- An admin-set maximum key lifetime.
