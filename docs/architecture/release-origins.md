# Release Origins

A released CLI must stay installable and updatable while GitHub is down.

Until this change every path to the binary ran through github.com — the
installer script, the version lookup, and the bytes. This document describes the
second origin, what still depends on GitHub, and how to turn the origin on.

## Why a second origin, read first

The CLI reads our own origin **before** GitHub, and falls back to GitHub
automatically. Reading it second would have been lower-risk on day one and worse
every day after: a fallback that only runs during an incident is a fallback
nobody knows works. Reading it first means every install and every update check
exercises the path, so a broken origin shows up on an ordinary Tuesday instead of
during an outage.

The fallback direction matters too. If our origin is unreachable, installs and
updates degrade to exactly the previous behaviour rather than breaking.

## The two origins

Both lay artifacts out identically, which is what lets one URL template cover
both:

| | Our origin | GitHub |
| --- | --- | --- |
| Archives | `https://install.entire.com/v0.10.0/entire_darwin_arm64.tar.gz` | `https://github.com/entireio/cli/releases/download/v0.10.0/entire_darwin_arm64.tar.gz` |
| Checksums | `https://install.entire.com/v0.10.0/checksums.txt` | `…/v0.10.0/checksums.txt` |
| Current version | `https://install.entire.com/channels/stable.json` | `https://api.github.com/repos/entireio/cli/releases/latest` |
| Install scripts | `https://install.entire.com/installers/install.sh` | `raw.githubusercontent.com/entireio/cli/refs/heads/main/scripts/install.sh` |
| Scoop manifest | `https://install.entire.com/scoop/entire.json` | `github.com/entireio/scoop-bucket` (git) |

### The channel pointer

`channels/stable.json` and `channels/nightly.json` answer one question — what is
the current version on this channel:

```json
{
  "channel": "stable",
  "tag": "v0.10.0",
  "version": "0.10.0",
  "published_at": "2026-08-12T19:07:28Z"
}
```

It deliberately carries **no checksums**. `checksums.txt`, which GoReleaser
generates and mirrors next to the archives, stays the single integrity source;
two files listing the same hashes are two files that can disagree.

Reading a pointer instead of the releases API also retires a live problem:
unauthenticated `api.github.com` allows 60 requests per hour per IP, which a
shared office or CI egress address can exhaust on its own.

Cache headers differ by what the object is. Anything under `v<version>/` never
changes once published and is served `immutable` for a year. The pointers change
on every release and get `max-age=60`.

## Who reads what

- **`scripts/install.sh`** — walks the origins for both version discovery and
  the download. Metadata requests carry `--max-time`; downloads carry
  `--speed-limit`/`--speed-time` instead, which aborts a stalled transfer
  without capping a slow but progressing one. `--connect-timeout` alone would
  not bound an origin that accepts the connection and then sends nothing, and
  that stall is the failure the fallback exists for. `--version X.Y.Z` pins a version and skips discovery entirely;
  `ENTIRE_INSTALL_BASE_URL` replaces the origin list with one entry, because
  someone pinning a mirror wants that mirror, not a silent fall back that hides
  whether it worked.
- **`scripts/install.ps1`** — the direct-install path uses the same origin
  order, metadata validation, and same-origin archive/checksum pairing as Unix.
  `-Version X.Y.Z` skips discovery and Scoop; if `-Channel` is also explicit,
  the pin must belong to it. `ENTIRE_INSTALL_BASE_URL` selects one authoritative
  mirror and the direct-install path, with no fallback to GitHub or Scoop.
  Metadata requests have a 60-second timeout. Downloads keep the existing
  host-specific stall handling (`Save-RemoteFile`); PowerShell 7.4+ has an
  explicit stall timeout, while older hosts retain their network defaults.
  Ordinary stable installs still use Scoop when available; `-InstallDir`,
  `-Version`, or an explicit mirror selects direct installation instead.
- **`cmd/entire/cli/versioncheck`** — `versionOrigins` is the ordered chain,
  `fetchLatestForChannel` walks it. `httpTimeout` (2s) bounds the *whole* chain,
  so adding an origin costs no extra worst-case latency on a path that runs
  synchronously after every command. Each candidate gets an equal share of
  what is left, so a primary that hangs cannot swallow the budget and starve
  the fallback — a firewall that blackholes rather than refuses would otherwise
  take update checks down for good, which is worse than not having the second
  origin at all.
- **Homebrew cask and the Scoop manifest** — still point at GitHub. See
  "Remaining work".

Two rules the chain borrows from `strategy.CheckpointReadRemotesWithElection`
and `checkpoint/remote/checkpoint_ref.go`, the closest precedents in this repo:

1. **Fail open.** Every origin failing is reported as one error, never as "you
   are up to date" or "there is nothing to install".
2. **A definite bad answer aborts; an unreachable one advances.** A missing or
   unreachable origin moves to the next candidate. A checksum *mismatch* does
   not — `install.sh` exits. Bytes that disagree with their own origin's
   checksums mean that origin is corrupt or hostile, and quietly retrying
   elsewhere would hide it.

Bad *metadata* is the other way round, and the distinction is the point. A
manifest naming something that is not a release version, or a version from the
other channel, **advances** to the next origin. At that moment we do not yet
know which version exists, so another origin may simply know better — advancing
is continuing discovery, not hiding corruption. By the time a checksum is being
checked the version is settled and only the bytes are in question, which is why
that case stops instead. Accepting unusable metadata would be the worst of both:
the chain ends, and because an invalid version sorts below every valid one, the
CLI reports no update at all while `install.sh` fetches a version that does not
exist. Both sides require the canonical `vX.Y.Z` form — `semver.IsValid` alone
accepts `v1.2`, which resolves to an artifact prefix that was never published.

Neither installer mixes origins within one install: the archive and the
`checksums.txt` that vouches for it always come from the same place. A healthy
origin's checksums vouching for another origin's bytes would be a weaker
guarantee than either origin offers alone.

The pointer write is **conditional on the object the guard inspected**
(`--if-match`, or `--if-none-match` for a first publish). Reading, comparing and
writing are three operations, so without it a second publisher landing in
between would pass its own guard and leave the pointer at whichever write
finished last — possibly the older one. A conflict aborts rather than retrying:
the guard exists to refuse unproven moves, and re-running is clearer in a
release log than a silent retry.

The workflow deliberately carries **no `concurrency` group**. GitHub keeps at
most one pending run per group and cancels the previously pending one — which
`cancel-in-progress: false` does not prevent, since it only protects the run
already in progress. Because `mirror-nightly` pushes a nightly tag, one stable
release can enqueue a second run and cancel a queued hotfix by itself. Dropping
a release outright is worse than the race, and the conditional write already
closes that race for workflow runs and manual ones alike.

The channel a tag belongs to is decided the same way everywhere: the literal
`-nightly.` prerelease segment goes to nightly, no prerelease to stable, and any
other prerelease is **refused at publish time**. The publisher, both installers,
`isNightly`, and the GitHub release scans all answer this question, and they have
to use the same test. A looser one in any of them routes a tag
like `v0.5.4-nightly` or `v0.5.4-prenightly` to the channel, where the readers
disagree about it: the CLI announces an update that `install.sh` then refuses to
install, which is worse than either accepting or rejecting it consistently.
All readers prefer `tag` over `version` and check the manifest's declared
`channel`.

**Publishing inverts rule 1 on purpose.** `publish-channel-manifest.sh` fails
*closed*: if it cannot read the current pointer it refuses to publish. The two
directions are not inconsistent, because the questions differ. A reader needs an
answer and the worst outcome is a confident wrong one, so it tries every origin.
The monotonic guard only has to answer "can I prove this move is forward", and a
pointer it could not read cannot prove it — so only a definite not-found counts
as an empty channel. Treating a permission error or a throttle as absence is
exactly what would let a re-dispatched old tag regress the channel for every
install and update check.

## Turning it on

The publishing side is **inert until `ENTIRE_RELEASE_BUCKET` is set**. That is
also the way back out: delete the variable and rerun the job, and the release
publishes exactly as it did before the mirror existed.

The GoReleaser gate tests the variable for **non-emptiness**, not for presence.
`release.yml` always exports it, and an Actions variable that is unset exports
as an empty string, so `isEnvSet` is true on every release. Gating on presence
would enable the `blobs` pipe with an empty bucket and no credentials —
`configure-aws-credentials` is correctly skipped — and fail every release
instead of staying inert. `scripts/test-release-origins.sh` pins this.

| Variable | Example | Effect |
| --- | --- | --- |
| `ENTIRE_RELEASE_BUCKET` | `entire-release-origin-4zovktgk` | Enables the GoReleaser `blobs` pipe, the installer upload and the channel-pointer step |
| `ENTIRE_RELEASE_UPLOAD_ROLE` | `arn:aws:iam::934711778961:role/github-actions-release-upload` | Role assumed via OIDC |
| `ENTIRE_RELEASE_REGION` | `us-east-2` | Optional, defaults to `us-east-2` |

The bucket name is not the hostname. The bucket is private and reached only
through the Worker, so it follows the infra repo's random-suffix convention
rather than being named after `install.entire.com`.

Do **not** add an `environment:` to the release job. That changes the OIDC `sub`
claim, and the upload role's trust policy matches `repo:entireio/cli:ref:refs/tags/v*`
and `repo:entireio/cli:ref:refs/heads/main` exactly — it would start refusing the
token.

Order of operations inside `release.yml` is load-bearing:

1. GoReleaser uploads the artifacts (`blobs`).
2. The macOS Developer ID check passes.
3. The channel pointer is flipped.
4. The install scripts and the Scoop manifest go to `installers/` and
   `scoop/` — stable releases only.

The pointer comes after 1 and 2 because it is what makes a version visible, and
advertising a release whose binaries Gatekeeper kills would be worse than not
advertising it at all.

Step 4 comes after the pointer rather than before it, which is the opposite of
what it looks like it should be. Those keys are **shared across versions**, so
uploading them first means a release the pointer then refuses — an older tag
re-dispatched — leaves the installers rolled back with nothing advanced. The
installer is version-independent, so a pointer that moved before it is harmless;
the reverse leaves a silent rollback behind a failed job.

Step 4 is stable-only because `installers/install.{sh,ps1}` and
`scoop/entire.json` are overwritten on every release rather than versioned — a
nightly publishing there would replace what stable users fetch. Nightly users run
the same stable installer with `--channel nightly`. For the same reason those
keys get `max-age=300` rather than the artifacts' immutable TTL: a long max-age
on an overwritten key pins a stale copy in every edge cache for its lifetime.

## The origin, as built

Built in `entirehq/infra` and verified end to end: an object uploaded to the
bucket was served through `install.entire.com` with the right bytes.

- **Bucket** `entire-release-origin-4zovktgk`, `us-east-2`, account
  `934711778961`. It is **private** — no anonymous read, no ACLs.
- **`install.entire.com` is a Cloudflare Worker.** It maps `/<path>` to the
  bucket key `<path>` verbatim and reads through its own OIDC role.
- **Upload role** `arn:aws:iam::934711778961:role/github-actions-release-upload`,
  trusting `repo:entireio/cli:ref:refs/tags/v*` and
  `repo:entireio/cli:ref:refs/heads/main` exactly. It can `PutObject` and
  `GetObject` under `v*`, `channels/*` and `installers/*`, plus `ListBucket`.
  No delete.

Two consequences worth keeping in mind when changing anything here.

**The Worker knows nothing about the layout.** It does not parse versions, split
channels or rewrite paths. That is what keeps this cheap to change: a new prefix
needs an IAM change and nothing else, and a layout change never touches the
Worker. The only place it may learn the layout is metrics dimensions, which have
to parse `v<version>/<file>` — keep that a best-effort regex that degrades to
`unknown` and never affects the response, so a layout change can blur a metric
but can never break a download.

**The bucket is private, so anonymous read is not the fallback it looks like.**
If the Worker is down, the bucket is unreachable; there is no bypass URL. The
fallback for that case is the same one as for a GitHub outage — the other origin
— which is why the chain reads two origins rather than two paths to one.

### Scoop without a bucket

Scoop does not require git. `scoop install <url-to-manifest.json>` installs from
a URL with no bucket added, records that URL as the app's source, and
`scoop update` re-fetches it and upgrades in place. Both verified against Scoop
v0.6.0.

So `scoop/entire.json` is mirrored here with its artifact URLs rewritten to this
origin — Scoop has no fallback of its own, so a mirrored manifest naming GitHub
would depend on the thing it exists to route around. The rewrite fails the
release if it matches nothing, because a manifest that still named GitHub would
publish cleanly and only be discovered during an outage.

The bucket manifest keeps its GitHub URLs on purpose. The ordinary
`scoop install entire` path is unchanged, so this is an additional GitHub-free
route rather than one that makes every Scoop install depend on a single origin.

One trap for anything generating these: Scoop rejects a manifest written with a
UTF-8 BOM, and its error for that is `Error parsing JSON`, followed by a
suggestion to run `scoop bucket add` — which reads like the URL was never
supported at all.

## Installing while GitHub is down

`install.sh` and the direct-install path in `install.ps1` handle this
automatically once the origin is live. The manual paths, for when they do not:

```bash
# Straight from our origin, no version lookup
curl -fsSL https://install.entire.com/v0.10.0/entire_darwin_arm64.tar.gz | tar -xz

# Pin a version through the installer
curl -fsSL https://entire.com/install.sh | bash -s -- --version 0.10.0

# From the Go module proxy, which caches independently of GitHub
go install github.com/entireio/cli/cmd/entire@v0.10.0

# Windows, via Scoop, with no bucket and no git
scoop install https://install.entire.com/scoop/entire.json
```

The `go install` route works today, with no infrastructure at all:
`proxy.golang.org` serves module content from its own cache and
`versioninfo.Resolve` recovers the real version from build info. Pin an explicit
version — `@latest` re-resolves against GitHub. The caveats are real but small:
no PostHog key is stamped in, so telemetry is off. Experimental commands follow
the resolved version, so a pinned stable tag hides them as a release does.

## Remaining work

- **Set the repository variables and watch a release.** Nothing has assumed the
  upload role from GitHub yet. If `configure-aws-credentials` fails the release
  job fails *before* GoReleaser runs, so do this on a release someone is
  watching. Back out by deleting `ENTIRE_RELEASE_BUCKET` and rerunning.
  Afterwards confirm: `channels/stable.json` names the new version,
  `v<version>/checksums.txt` returns 200 with
  `cache-control: public, max-age=31536000, immutable`, and
  `installers/install.sh` returns 200.
- **Tell the infra side once the first release with the installer step ships.**
  `entire.com/install.{sh,ps1}` still redirect to raw GitHub; infra repoints them
  at `install.entire.com/installers/…` only after a release has actually uploaded
  those keys. Repointing first would turn every install into a 404 with no
  fallback.
- **Repointing the cask and bucket download URLs is not an outage fix**, and is
  deliberately not done. Both manifests live in GitHub git repos
  (`entireio/homebrew-tap`, `entireio/scoop-bucket`), so `brew update` and a
  bucket refresh need GitHub whatever the artifact URL says. Changing
  `homebrew_casks[].url.template` or `scoops[].url_template` would only help
  someone who refreshed metadata *before* the outage and has not installed yet —
  a narrow window. It would move download bytes off GitHub, which is a bandwidth
  and partial-outage argument rather than an availability one, and it would put
  every cask and bucket install behind a single origin with no fallback of its
  own.

  The complete path is the one `scoop/entire.json` takes: mirror a manifest whose
  URLs name this origin, so metadata *and* bytes come from us. Scoop supports
  that directly (`scoop install <url>`, no bucket, no git). Homebrew has no clean
  equivalent, so the cask stays outage-affected; `install.sh` is the GitHub-free
  path on macOS.
- **Decide on signing.** Nothing is signed today beyond macOS notarization — no
  cosign, no SBOM, no attestations. A second origin doubles the number of hosts
  that can serve a bad binary. Two options: sign `checksums.txt` with a key
  pinned in the installer and the CLI, or fetch `checksums.txt` from both origins
  and require agreement, degrading to one origin when the other is down.
- **Warm the module proxy** at release time (a `GET` of
  `proxy.golang.org/github.com/entireio/cli/@v/vX.Y.Z.info`) so the `go install`
  escape hatch is populated *before* an outage rather than during one.
- **Measure the install mix.** `installManagerForCurrentBinary()` already detects
  brew/mise/scoop/unknown but never reports it, so how much of the user base each
  path covers is currently inferred from `checksums.txt` download counts. See
  `docs/install-provenance-plan.md`.

## Tests

`scripts/test-release-origins.sh` (also `mise run test:release-origins`, and a
job in `ci.yml`) runs both scripts against local fake origins: version discovery,
download and verification from our origin, fallback to GitHub for both, a
checksum mismatch aborting rather than falling back, `--version` pinning, the
all-origins-down error, and the channel pointer's routing, monotonicity and
idempotence. It is hermetic and refuses to run an installer still aimed at a
public origin.

`mise run test:ps1` runs the PowerShell origin tests under `scripts/test/`,
including metadata rejection/fallback, literal nightly routing, pinned-version
validation, authoritative mirror selection, same-origin checksum pairing, and
fatal checksum mismatches. Network calls in unit tests are mocked; local HTTP
fixtures exercise the real JSON decoder and download functions. CI runs the
suite on both Windows PowerShell 5.1 and pwsh.
