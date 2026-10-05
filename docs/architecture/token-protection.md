# Secure Enclave token protection

Opt-in, macOS only. `entire auth protect` seals every saved login to a key in
the Mac's Secure Enclave. From then on each use of a login shows the macOS
Touch ID (or password) dialog naming the action, so an agent running
`git push` or `entire trail create` cannot spend the user's login silently.

## What is stored where

| Item | Location | Prompts |
| --- | --- | --- |
| Enclave key blob | `<config dir>/token-key.sekey`, mode 0600 | never |
| Access + refresh token bundle | the context's access slot in the keyring, as `se1:<base64>\|<expiry>` | on read |
| Refresh slot | cleared while protected | n/a |

The key is created with `kSecAttrIsPermanent=false` and exported as its
token object id (`toid`). That blob is wrapped by this Mac's enclave and
carries the access-control policy (`userPresence`) inside it, so any process
that loads it still hits the dialog. Nothing is added to the keychain's
data-protection class, which is what lets an unsigned `go build` binary use
the enclave: keychain-resident enclave keys need a `keychain-access-groups`
entitlement, and that entitlement is restricted on macOS. A bare CLI binary
cannot carry the provisioning profile it needs, so ad-hoc and Developer ID
signed binaries carrying it are killed at launch. The blob route sidesteps
that entirely.

Sealing is ECIES to the public half (`SecKeyCreateEncryptedData`) and never
prompts, so login and refresh rotation stay silent. Unsealing goes through
`SecKeyCreateDecryptedData` on the private half and prompts. All calls are
bound at runtime with purego; release builds stay `CGO_ENABLED=0`.

## Code map

- `internal/entireclient/tokenstore/senclave`: `Generate`, `Load`, `Seal`,
  `Unseal(ciphertext, reason)`, `PublicKey`. Darwin only; other platforms
  return `ErrUnsupported`.
- `cmd/entire/cli/auth/protected.go`: bundle format, `EnableProtection`,
  `DisableProtection`, `TokensProtected`, `WithPromptAction`,
  `PromptDeclined`, and the `protection` sealer source tests swap with
  `SetSealerForTesting`.
- `cmd/entire/cli/auth/refresh.go`: `contextTokenStore` loads and saves the
  sealed bundle for auth-go's token manager. One load is one prompt because
  both tokens live in one bundle.
- `cmd/entire/cli/auth/contexts.go`: `RecordLoginContext` writes sealed when
  protected; `LoginTokenForContext` unseals.
- `cmd/entire/cli/auth_protect.go`: the `auth protect` and `auth unprotect`
  commands, registered as experimental.
- `cmd/git-remote-entire/main.go`: attaches "git push to <host>" or
  "git fetch from <host>" to the request context so the dialog names it,
  and ignores `ENTIRE_TLS_SKIP_VERIFY` while protected.

## Dialog text

macOS renders `"<binary>" is trying to <reason>.` The reason is
`<action> with Entire login <handle>@<login server host>`, for example
`git push to us.entire.io with Entire login toothbrush@us.auth.entire.io`.
CLI commands derive the action from their resolved Cobra command path
(`entire trail list`). macOS draws the dialog and fills in the binary name
of the process asking; the reason is text the caller supplies. Another
process that loads the key blob gets a dialog under its own name with its
own reason (see the threat model below), so the wording tells the user what
Entire is doing, it does not prove Entire is the one asking.

## Prompt count

One process unseals a given slot at most once. `unsealedCache` in
`protected.go` remembers every bundle the process unsealed or sealed, keyed
by the slot's exact encoded value. A command builds several token managers
and auth-go re-reads the store after taking its refresh lock; without the
cache each of those reads was a dialog. A slot rotated by another process
has new ciphertext and prompts again.

| Operation | Prompts |
| --- | --- |
| `entire login`, token refresh | 0 |
| `entire <command>` touching the API | 1 |
| `git fetch` / `git pull` | 1 |
| `git push` with checkpoint sync on | 1 (nested pushes use the per-push unlock below) |
| agent hooks (commit, session) | 0 (they never read tokens) |

## Per-push unlock

The pre-push hook pushes checkpoint refs with its own `git push` calls,
each a new helper process, and falls back to one push per ref with
fetch-and-replay when the batch is rejected. Every one of those would be a
dialog. Instead the helper that passed the dialog serves its unsealed
bundles over a Unix socket (`unlock.go`), and a nested helper asks before
prompting (`openSealedSlot`).

- Git runs a remote helper through a `git remote-entire` wrapper process, so
  the helper's parent is that wrapper and the user's `git push` is one level
  up. The server therefore opens one socket per ancestor whose process name
  is `git`, at `<cache dir>/unlock/git-<pid>.sock`. Shells and agents above
  git get none. A client walks its own parent chain looking for a socket
  named after each ancestor; the user's push is the one nested helpers
  share with the outer helper.
- The server reads the peer's pid and uid from the kernel (`LOCAL_PEERPID`,
  `LOCAL_PEERCRED`) and walks the peer's parent chain through
  `kern.proc.pid` until it reaches the socket's anchor. It refuses when the
  uid differs, the chain never reaches the anchor, any process in the chain
  started before the anchor did, the anchor is no longer above the server
  itself (git exited and the helper was reparented), or the anchor pid's
  start time changed (pid reuse).
- Only a process that unsealed through the dialog serves. A nested helper
  that received its bundle over the socket does not.
- The plaintext never touches disk. The socket node is 0600 inside a 0700
  directory, but the peer check is the control; the mode is a courtesy.
- The server stops and removes the socket when the helper exits. A stale
  node from a crashed helper is replaced on the next start.

What the unlock does not cover: anything the repo already runs during
pre-push is a descendant of the user's git and passes the ancestry check.
That code already executes with the user's rights, so the unlock widens
nothing, but it scopes the approval to "this push", not to Entire's own
binaries. Reading the serving helper's memory needs the hardened-runtime
release signing; a plain `go build` dev binary is attachable by a same-user
debugger.

## Threat model

Protected against, with user-level code execution on the Mac:

- Reading tokens from the keychain or `security find-generic-password`: the
  slot holds ciphertext.
- Running `git push` or `entire` commands: the dialog names the action.
- Editing `contexts.json` to point a context at an attacker-controlled login
  server: the bundle carries its issuer and handle and is refused on
  mismatch (`ErrBundleMismatch`).
- Intercepting TLS via `ENTIRE_TLS_SKIP_VERIFY` or `--insecure-http-auth`:
  both are ignored while protected. Loopback `http://` cores stay allowed.
- Turning protection off: `auth unprotect` has to unseal first, which prompts.
- A plaintext slot while a key is enrolled (interrupted enrolment, a legacy
  writer such as the entiredb CLI): reads fail closed with
  `ErrPlaintextWhileProtected` and point at `entire auth protect`, which
  seals it. `sealContext` clears the plaintext refresh slot before writing
  the bundle, and clears a stray one even when the access slot is already
  sealed.
- A cancelled dialog partway through `auth unprotect`: the logins already
  unsealed are sealed again from the process cache, without a prompt, so the
  keyring never sits half protected while the key is enrolled. The command
  reports that nothing changed.
- Concurrent `auth protect` or `unprotect` runs: both hold
  `token-key.lock` in the config dir (`flock.AcquireIn`), so two runs cannot
  mint different keys and seal slots the other cannot open.
- A key blob that will not load (a config dir synced to a machine without
  an enclave, or a damaged file): every token read and write fails closed
  with `ErrKeyUnusable` rather than falling back to plaintext. `auth
  unprotect` removes the blob without unsealing and names the logins left
  sealed under it, which need `entire login`.

Not protected against:

- A process that loads the key blob itself and shows its own dialog text.
  The blob is readable by the user, and the enclave enforces presence, not
  which binary asked. Binding to our signed binaries needs the keychain
  route, which needs an app bundle with a provisioning profile.
- Replay of a captured login JWT until it expires (one hour). Making the
  bearer useless without a fresh enclave signature needs DPoP (RFC 9449) on
  the login server and data plane: bind tokens to this key, sign a proof per
  request. The key and plumbing here are the client half of that.
- Root or kernel compromise.

## Platform notes

- No GUI session (SSH, headless): unseal fails with `ErrNoInteraction`,
  mapped to `auth.ErrPromptDeclined`. Commands fail closed and say so.
  `ENTIRE_TOKEN` still bypasses by design; it is a separate credential.
- The entiredb CLI shares the keyring prefix and will read ciphertext from
  a protected slot. Port `senclave` there or document the breakage before
  shipping to users who run both.
- Dev builds: plain `go build` works. No signing or entitlements needed.

## Manual verification

```
go build -o entire ./cmd/entire && go build -o git-remote-entire ./cmd/git-remote-entire
./entire auth protect
./entire auth status            # expect one Touch ID dialog, "protection" row
git -c core.sshCommand= fetch   # in a repo with an entire:// remote; one dialog
./entire auth unprotect
```
