# External Commands

## Overview

The Entire CLI supports kubectl-style external commands — standalone binaries on `$PATH` that extend the CLI without modifying the main repository. When the user invokes `entire <name>` and `<name>` isn't a built-in subcommand, the CLI looks for an `entire-<name>` binary on `$PATH` and execs it with the remaining arguments. Stdio passes through, exit codes propagate, and the parent CLI does no further processing of the child's output.

This is **not** the same mechanism as the [external agent protocol](external-agent-protocol.md). External commands have no protocol, no JSON contract, no lifecycle hooks. Use the agent protocol when you need checkpoint integration; use external commands for everything else.

## Resolution

The CLI does not scan `$PATH` at startup. Resolution is lazy: when `os.Args[1]` doesn't match a built-in subcommand, the CLI calls `exec.LookPath("entire-" + os.Args[1])`. If a binary is found and executable, it runs before Cobra parses arguments.

Rules, in order:

1. **Built-ins win.** If the first argument matches a Cobra subcommand (or one of its aliases), the external command is never considered.
2. **Reserved names are skipped.** Names beginning with `agent-` are reserved for the [agent protocol](external-agent-protocol.md). The resolver refuses to invoke them as external commands.
3. **Path-traversal candidates are rejected.** Names containing `/` or `\` never resolve.
4. **Found-but-not-executable surfaces as a launch error.** If `entire-<name>` exists on `$PATH` but lacks the executable bit, the resolver reports `Failed to run plugin entire-<name>` with exit code 1, rather than falling through to Cobra's "unknown command" path.

### Managed install directory

Users can drop binaries anywhere on `$PATH`, but a per-user managed directory is also automatically discovered:

- **Default:** `$XDG_DATA_HOME/entire/plugins/bin` (Linux/macOS) or `%LOCALAPPDATA%\entire\plugins\bin` (Windows).
- **Override:** `$ENTIRE_PLUGIN_DIR/bin`.

The CLI prepends this directory to `$PATH` at startup via `cli.PrependPluginBinDirToPATH()` so the existing `exec.LookPath` resolution finds managed installs without any special-casing. This is purely additive — the kubectl-style `$PATH` model is unchanged.

`entire plugin install/list/remove/upgrade` manage the contents of this directory. Authors who prefer the raw "drop a binary on `$PATH`" model don't need to use it.

### Remote install

`entire plugin install` accepts three source forms:

| Form | Example | Behavior |
|---|---|---|
| bare name | `entire plugin install run` | Resolved through the [plugin index](#plugin-index-discovery) |
| repository URL | `entire plugin install https://github.com/entireio/entire-run` | Installs from any git host. Also accepts git's scp-like form with any SSH username (`deploy@git.corp.io:group/entire-foo.git`) — the same set `validatePluginRepoURL` allows |
| local path | `entire plugin install ./dist/entire-run` | Symlink/copy into the managed dir (unchanged) |

Remote installs are deliberately forge-agnostic:

1. **Version resolution** uses `git ls-remote --tags` — identical on GitHub, GitLab, Gitea/Forgejo, and self-hosted servers; inherits the user's git auth and proxy config. The highest **stable** semver tag wins; `--pin <tag>` installs exactly that tag and marks the manifest pinned (skipped by `upgrade`).

   Prereleases are excluded from resolution. semver ranks `v2.0.0-rc1` above stable `v1.9.0`, so listing them would move every user onto a release candidate on the next `plugin upgrade --all` the moment an author pushed one. `--pin` is the opt-in, since it bypasses listing entirely. Every git subprocess also carries a two-minute deadline — the root context is cancellable but has no timeout, so an unreachable host would otherwise hang the command with no output, which matters most during dependency planning against author-controlled URLs.
2. **Metadata** is read from `entire-plugin.yml` at the repo root via a blobless shallow clone (with a plain shallow-clone fallback for servers that don't allow partial-clone filters). The file is optional; without it the plugin name derives from the repo basename (`entire-run` → `run`).
3. **Asset download** is the one forge-specific step, contained in a small URL-convention table: GitHub/Gitea-style `<repo>/releases/download/<tag>/<asset>`, GitLab-style `<repo>/-/releases/<tag>/downloads/<asset>`, unknown hosts default to GitHub-style. Authors on other hosts declare a `download_url` template in `entire-plugin.yml` (placeholders: `{name}` `{tag}` `{version}` `{os}` `{arch}` `{asset}`).
4. **Asset selection** goes through the release's `checksums.txt`: the manifest lists what was actually published, and the download is verified against it. Candidate names follow goreleaser conventions (`entire-<name>_<version>_<os>_<arch>.tar.gz` and friends, with `x86_64`/`aarch64` aliases and a `darwin_all` universal-binary fallback (`all` occupies the *arch* slot, which is where goreleaser puts it)). A pushed tag with no published assets falls back to the next-highest tag with a warning.
5. The binary lands in `pkg/<name>/` next to a `manifest.yml` — written atomically, and *before* the `bin/` link — recording provenance (repo URL, tag, asset, asset SHA-256, **binary SHA-256**, verification state, pin state, dependency list), and is linked into `bin/` through the same symlink→hardlink→copy fallback as local installs. The dispatcher never changes.

The manifest is written immediately after the binary swap and before the `bin/` link, which is ordering that matters rather than style. `replaceBinary` has already mutated `pkg/<name>/`; until the manifest catches up it records the *previous* tag and `binary_sha256` while the new binary is on disk, and `doctor`'s integrity check reads that as tampering — a permanent false alarm on a healthy install. Writing it first leaves only a local re-hash in that window, and if the link then fails, `doctor` reports the real problem ("has an install manifest but no entry in the managed bin dir") with a fix that works.

#### A plugin gets exactly one binary, and does not choose its name

The archive entry's name only *selects* what to extract. The destination is built from the validated plugin name — always `entire-<name>[.exe]` — so what lands in `pkg/<name>/` and `bin/` cannot be anything else, and `validatePluginName` has already rejected separators, `.`/`..`, a leading `-`, and the reserved `agent-` prefix. Every extraction branch returns on its first match, so one install writes one binary; other archive members, including a *differently* named `entire-*` one, are ignored. A local `install ./path` is gated the same way, refusing any source whose basename lacks the prefix.

Selection among candidates is deterministic: shallowest path first, then lexicographic. Matching is by basename, so an archive can legitimately hold several — the binary at the root, the same name nested under a versioned directory (goreleaser's `wrap_in_directory`), and unrelated files such as `completions/entire-<name>`. Taking whichever came first made the installed binary depend on the order the author's tar was built in. For tar this costs a second pass, since the stream cannot seek backwards.

The two digests are not redundant. `sha256` covers the downloaded asset, which is usually an archive and is discarded with the staging directory — provenance only, nothing left to compare against. `binary_sha256` covers the executable under `pkg/<name>/`, which is what `entire plugin doctor` re-hashes to detect a binary swapped out after install.

Installing from a URL not listed in the index prints the source and asks for confirmation (`--yes` to skip; required in non-interactive runs). A URL that *is* listed installs without prompting, and takes its expected name from the catalog entry — otherwise the remote would name itself unchallenged on a path with no prompt at all, and `--force` would replace whichever plugin it named.

`--force` still does what it says, including moving a plugin to a new repository (`entire-sem` → `entire-graph` is a real case). What it must not do is replace a plugin the user never named: a URL install's confirmation names a URL, and the remote's `entire-plugin.yml` decides which `pkg/<name>/` is overwritten. So a `--force` install that displaces a plugin from a *different* repository reports what it replaced and where that came from, which is the URL to put things back. Index-listed repos install without prompting — with one exception: `entire plugin browse` confirms its selection. The picker shows only a name and description, and an index-resolved install would otherwise proceed unprompted, so highlighting a row and pressing Enter would download a binary and link it onto `PATH` in a single keystroke. The confirmation names the repository the binary comes from.

#### Repository URLs are a security boundary

Every repo URL is validated by `validatePluginRepoURL` before it reaches the git CLI, and every git invocation passes `--` before its positionals. Both layers exist because repo URLs are attacker-influenced: they arrive from `index.json` entries and from `--index`/`ENTIRE_PLUGIN_INDEX_URL`.

git parses an option-shaped positional as an option, and `--upload-pack=<cmd>` is shell-interpreted — with no positional repository left, it runs against the *ambient* repo's `origin`. So a catalog entry like `--upload-pack=curl … | sh; git-upload-pack` would execute arbitrary commands during an index-resolved install, which by design never prompts. Dependency planning is the tightest path: it contacts a dependency's repository — now always an index-listed one — *before* the install confirmation.

Accepted forms are `https://`, `ssh://`, and git's `user@host:path` scp-like syntax. Anything else — a leading `-`, a bare name, a relative or absolute path, `file://`, git's command-executing `ext::` transport — is rejected.

`file://` is accepted **only under test**. Nothing in production needs it: a `file://` plugin repository cannot complete an install, because `releaseAssetBaseURL` requires an http(s) repo URL to locate the asset, and `install ./path` is the supported way to use a local build. Its only real consumer was the test suite, and a security allowlist should not be widened to pay for test convenience — the alternative transports a test could use are worse, since `git daemon` serves `git://`, exactly the scheme rejected above. The gate is `testing.Testing()` plus `ENTIRE_TEST_ALLOW_FILE_REMOTES` for the integration harness, which spawns the real binary where `testing.Testing()` is false. Being able to set that variable already implies code execution, so it is not a weakening; what it buys is that a hostile catalog entry cannot name a local path and use the CLI to probe the filesystem.

`http://` and `git://` are deliberately absent. Both are unauthenticated and unencrypted, and the catalog fetched over them decides which repositories install *without a confirmation prompt*, so an attacker who can rewrite the transport chooses the binary — strictly worse than the plaintext-asset case below. Bare absolute paths are rejected too: `install ./path` is the supported way to install a local build, and it goes through the filesystem branch rather than handing a path to git as a remote.

The rule has exactly one definition, `validatePluginRepoURL`, applied to plugin repositories and to the index URL alike. Two validators previously disagreed about `http://`, `git://`, and absolute paths, so `--index /srv/idx` was accepted while the equivalent settings value was a hard load failure; the settings key is gone (see below) and one definition remains. Invalid index entries are dropped like invalid names, so one bad row cannot take out the catalog.

This mirrors `validatePluginName`, which refuses a leading `-` on names for the same reason. Regression coverage asserts the payload never executes, not merely that the call errors.

#### `entire-plugin.yml` decodes leniently

Unknown keys are ignored, the same choice made for `index.json` and for the same reason: both are artifacts read by every CLI version ever shipped, and refusing one an older binary doesn't fully understand breaks it permanently for everyone on that version, with no way for the plugin author to fix it for them. `entire-plugin.yml` has no `version` field to gate on either, so the first author to adopt any future field would break installs on every older CLI.

This gives up catching author typos, which strict decoding did. The trade is asymmetric: a misspelled key costs the author one confused test run against their own plugin; a forward-compatibility break is unfixable and fleet-wide. Author-side validation belongs in a lint command, not in the path every user's install runs through.

An empty or comment-only file is also accepted — the file is documented as optional, and a committed placeholder should not be fatal at every tag.

#### The installed name must match what was requested

The installed plugin name comes from the *remote* — `entire-plugin.yml`'s `name:`, else the repository basename. Whenever the caller has already committed to a name, that name is passed down as a required argument to `InstallPluginFromRepo` and a mismatch is fatal. It is an argument rather than an options field deliberately: a field can be silently omitted, and omitting it reopens the hole, whereas passing `""` is a visible choice at the call site. Three paths set it: an index-resolved install (the catalog entry the user typed), a dependency install (the requirement being satisfied), and an upgrade (the plugin being upgraded). A bare `install <url>` sets nothing, because there the repository legitimately names itself.

Unchecked, the remote chose the name unilaterally and three things broke: an index entry named `safe` could install `entire-hijack` with no prompt (index installs never prompt); `--force` escalated across plugins, because the already-installed check tested the remote-declared name, so reinstalling A let A's repo declare `name: B` and replace an unrelated installed B; and a dependency installed under another name never satisfied its requirement, so `doctor` reported it missing forever and every future parent install re-attempted it.

It fails rather than warns: every caller that sets an expectation has already made a trust decision about *that* name, and silently honoring a different one voids it. A genuine rename is a catalog entry or a requirement to fix.

#### Credentials never reach logs, output, or disk

A remote may embed credentials (`https://user:token@host/repo`). Every path that prints, logs, or persists a repository, index, or **asset download** URL strips the userinfo first, and git's captured stderr is scrubbed for the same pattern.

The download path needs saying explicitly: `releaseAssetBaseURL` derives the asset URL from the repo URL, and `url.String()` re-serializes embedded userinfo, so a private-forge remote produces a credentialed download URL. The request keeps it — that is how it authenticates — but every message goes through `redactURL`, because a download failure is an ordinary event (network hiccup, 5xx, checksum mismatch) and `main.go` prints command errors straight to stderr. This matters more than usual here: `.entire/logs/` lives inside the user's working tree and is collected wholesale by `entire doctor bundle`, and `manifest.yml` is written mode 0644. Upgrades re-resolve auth through git's credential helpers, which is where it belongs.

#### Downloads must be authenticated, over a transport that can't be rewritten

Two requirements apply to every asset fetch, both enforced at the HTTP boundary so the author-declared `download_url` escape hatch is covered as well as the derived forge URLs:

**HTTPS.** Plaintext HTTP is refused for anything off-machine. This isn't stylistic: the asset and the `checksums.txt` that authenticates it come from the same origin, so an attacker who can rewrite one can rewrite both and checksum verification proves nothing. Loopback (`127.0.0.1`, `::1`, `localhost`) is exempt so `httptest` fixtures and local forge experiments keep working — traffic that never leaves the machine has no network attacker to defend against.

The check runs on **every redirect hop**, via the client's `CheckRedirect`, not just the initial URL. Go's default policy follows up to ten redirects and will happily cross from `https` to `http`, so validating only the entry point left the rule bypassable by any release host with an open redirect — the asset *and* its `checksums.txt` could both arrive in plaintext, which is precisely the outcome the rule exists to prevent.

**A checksum manifest.** A release that publishes an asset but no `checksums.txt` covering the platform is refused by default; installing means making those bytes executable, so it takes an explicit `--allow-unverified`. Two shapes are unverifiable and hit the same gate: a release with no manifest at all, and a `download_url` with no `{asset}` placeholder (every candidate name expands to one fixed URL, so no manifest can be located — add `{asset}` to fix it properly).

The candidate probe still runs when verification is required, because it separates two failures that need different handling: *no release published for this tag yet* (`errAssetNotFound` — worth walking down to the next-highest tag) from *release exists but publishes no checksums* (`errUnverifiedAsset` — an older tag wouldn't be any more trustworthy, so the walk stops). Conflating them would report a missing release for a plugin that simply doesn't ship checksums.

An install that used the opt-in is recorded as `unverified: true` in the manifest, reported by `entire plugin doctor` until the author publishes checksums, and inherited by `plugin upgrade` — so a knowingly-unverified plugin doesn't start failing on upgrade, and a verified one can't silently become unverified.

### Plugin index (discovery)

Discovery rides on a git-synced index, krew-style: the index is itself a git repository containing `index.json`, shallow-cloned into the user cache (keyed by a hash of the URL) and refreshed on a TTL. `entire plugin search [term]`, `info <name>`, and `browse` read it; `entire plugin index update` forces a refresh. When a refresh fails but a cached copy exists, the stale copy is used — discovery doesn't hard-fail offline.

The effective index URL resolves as: `--index` flag > `ENTIRE_PLUGIN_INDEX_URL` > the built-in default (`https://github.com/entireio/plugin-index`). Freshness is a fixed 24 hours.

**Repo-level settings are deliberately not a source, and there is no TTL knob.** Both were removed rather than hardened:

`plugins.index_url` was read from `.entire/settings.json` — a committed file resolved from the working directory — while an index-listed repository installs with *no confirmation prompt*. Composed, that meant a cloned repository could redirect the catalog and have an attacker-chosen binary downloaded, `chmod 0755`, and linked onto the user's `PATH` without a prompt. The value of the setting was precisely that contributors got a different catalog *without knowing*, which is the vulnerability stated as a feature: it cannot be kept and made safe, only removed or made visible. Removing it also makes the failure mode safe — a missing override now falls back to the curated default rather than to someone else's catalog.

An organization wanting an internal catalog sets `ENTIRE_PLUGIN_INDEX_URL`, which applies across repositories rather than per-repository and cannot be chosen by content the user merely checked out. Go takes the same position — no repo-committed file can redirect `GOPROXY` — while npm's per-project `registry` override is the cautionary counter-example.

`plugins.index_ttl_hours` went with it: `entire plugin index update` already forces a refresh and stale-on-offline already covers a failed one, so the knob tuned a problem solved twice while costing a settings load per sync. Between them, the whole `PluginSettings` struct, its validator, its merge path, and its tests are gone — the plugin layer now reads no settings at all.

`index.json` schema (version 1): `{"version": 1, "plugins": [{"name", "repo_url", "description", "official", "platforms"}]}`. Entries with invalid names (e.g. the reserved `agent-` prefix) or unusable repo URLs are filtered on load, not fatal.

`version` is **recorded, not enforced**: an index declaring a newer version (or omitting the field) still loads, logging a warning. Gating on it would guard a migration that can never happen — the index is one shared resource read by every CLI version ever shipped, so bumping it would break discovery fleet-wide with no gradual rollout and no undo for installed binaries. An incompatible schema therefore ships at a new path (`index-v2.json`, another branch, another repo). Meanwhile the changes that do happen are already absorbed: decoding ignores unknown fields, so new fields are free, and unreadable entries are dropped individually. Degrading per entry beats refusing the catalog — a company's hand-written internal index that forgets `"version"` should not be told to upgrade the CLI.

### Dependencies

A plugin declares dependencies in `entire-plugin.yml`:

```yaml
name: brain
requires:
  - name: graph          # resolved by name through the plugin index
    min_version: v0.2.0  # minimum only; no ranges, and validated at parse
```

`min_version` is checked for well-formedness when the metadata is parsed. A malformed value would otherwise *remove* the floor rather than fail: `x/mod/semver` ranks an invalid string below every valid one, so the comparison in `dependencySatisfied` reports any installed version as acceptable — `vtypo`, `latest` and `>=1.0` all behave that way. Rejecting a malformed value of a known field is consistent with the lenient decoding above, which is about unknown *keys*: those are a newer CLI's fields and refusing them breaks older binaries permanently, whereas a bad version string is an author error no CLI version will ever accept.

**A requirement carries no URL.** A missing dependency resolves by name through the index and nowhere else, so the URL a dependency install fetches from always comes from the curated catalog rather than from the requiring plugin's author. With an author-supplied `repo_url`, installing one plugin meant fetching and executing a binary from a URL its author chose — and planning contacted that URL *before* the confirmation prompt.

The capability is not gone, the authority moved: a user can still install an out-of-catalog dependency by URL themselves, with the usual untrusted-source prompt, after which the requirement is satisfied and planning schedules nothing. Consent belongs to the user, not the plugin author.

The consequence is a publishing order — a dependency must be indexed before a dependent can ship — which is the same constraint krew, Homebrew and apt impose. A `repo_url` left over from the old schema is ignored rather than honored, since `entire-plugin.yml` decodes leniently, so previously-published files keep parsing and simply lose the URL.

Resolution is **install-time only** — dispatch stays zero-cost. The outcome is verified, not just the attempt: an install that lands below the requirement's `min_version` fails with both versions named, because the install takes the newest published tag and that tag may still be too old. Reporting success there would defeat the guarantee the plan was built on. After installing the main plugin, missing dependencies are resolved transitively (metadata-only, nothing downloaded during planning; a per-name record of the strictest `min_version` seen, plus a depth bound, make metadata cycles an error rather than a hang), listed apt-style, and installed after one confirmation (`--yes` skips, `--no-deps` opts out). The listing carries no per-entry trust annotation, because there is no longer a path by which an author-chosen URL can appear in it: a missing dependency comes from the index, and an upgrade reinstalls from the source that plugin's own install already accepted. A declined plan is a warning, not a failure. Dependencies already satisfied from raw `$PATH` or a local-dev install count as satisfied, with a warning when `min_version` can't be verified.

Planning tracks the strictest `min_version` seen per plugin rather than a plain visited set. In a diamond where two requirers demand different minimums of the same plugin (A needs `sem >= v1.0.0`, B needs `sem >= v2.0.0`), a name-only set would mark `sem` handled on A's satisfied requirement and skip B's stricter one entirely — no action, no warning — completing the install with B running against a too-old `sem`. `doctor` caught that afterwards, since it walks each manifest's requirements independently, but the install plans the upgrade instead of deferring the discovery. One action per plugin name either way. The requirement list is copied into the install manifest so reverse-dependency checks work offline:

- `entire plugin remove sem` refuses when another manifest requires it (`--force` overrides).
- `entire plugin doctor` reports missing/outdated dependencies, manifest/bin-dir drift, binaries that no longer match the `binary_sha256` recorded at install, installs that were never checksum-verified, dangling local-dev symlinks, and (macOS) a `com.apple.quarantine` attribute that would block execution. Exit code 1 when issues are found. The integrity check covers the `pkg/` binary the manifest describes; where `bin/` holds a copy rather than a link (Windows without Developer Mode), the dangling/non-executable link check is what guards that surface.

> **Compatibility note:** the `entire plugin` command group is itself a built-in. Per the "built-ins win" rule above, it shadows any external command named `entire-plugin` that may have existed on `$PATH` previously. The collision is intentional — managing plugins is a built-in concern — but worth flagging for anyone who shipped an `entire-plugin` external command before this layer landed.

## Environment

Each external-command invocation receives:

| Variable | Description |
|---|---|
| `ENTIRE_CLI_VERSION` | The CLI's version string (e.g. `0.42.0`, `dev`) |
| `ENTIRE_REPO_ROOT` | Absolute path to the git repository root, when the CLI is invoked inside one. Omitted otherwise. |
| `ENTIRE_PLUGIN_DATA_DIR` | Per-plugin durable storage directory (`<plugin-root>/data/<name>`). Not pre-created — the plugin should `mkdir -p` on first write. Set regardless of whether the plugin is on raw `$PATH` or in the managed dir, so plugins get the same contract either way. Omitted only in degenerate environments where the per-user data root cannot be resolved (e.g. no home dir, no `LOCALAPPDATA`/`XDG_DATA_HOME`/`ENTIRE_PLUGIN_DIR`); the parent CLI prints a warning to stderr in that case. |

The working directory is **not** changed — external commands run in the user's current directory, the same as any other shell command.

### Environment filtering

Unlike `kubectl` and `gh`, which forward the parent's full environment to every plugin, Entire **filters** the parent environment through a small allowlist before invoking an external command. The motivation is defense in depth: a plugin you installed shouldn't see `AWS_ACCESS_KEY_ID`, `GITHUB_TOKEN`, or `OPENAI_API_KEY` unless it has a reason to. (A malicious plugin can still read files under `$HOME` — the boundary is "what's accidentally exposed", not "what an attacker can reach".)

Variables forwarded by default fall into a few categories:

- **POSIX basics** — `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `PWD`, `TMPDIR`, `TZ`
- **Locale** — `LANG`, `LANGUAGE`, and the entire `LC_*` family
- **Terminal / color** — `TERM`, `TERM_PROGRAM`, `COLORTERM`, `NO_COLOR`, `FORCE_COLOR`, `CLICOLOR`, `CLICOLOR_FORCE`
- **CI detection** — `CI`, `GITHUB_ACTIONS`, `GITLAB_CI`, `BUILDKITE`, `CIRCLECI`, `JENKINS_URL`, `TEAMCITY_VERSION`, `TRAVIS`
- **Proxies** — `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, `ALL_PROXY` (and lowercase variants)
- **SSH agent** — `SSH_AUTH_SOCK`, `SSH_CONNECTION`
- **Windows essentials** — `SYSTEMROOT`, `WINDIR`, `APPDATA`, `LOCALAPPDATA`, `PROGRAMDATA`, `PROGRAMFILES`, `PROGRAMFILES(X86)`, `USERPROFILE`, `USERNAME`, `HOMEDRIVE`, `HOMEPATH`, `COMSPEC`, `PATHEXT`
- **Namespace prefixes** — anything starting with `ENTIRE_`, `LC_`, or `XDG_`

The full list lives in `pluginEnvAllowed` and `pluginEnvPrefixes` in `cmd/entire/cli/plugin_env.go`.

### Opting names back in: `ENTIRE_PLUGIN_ENV`

If a plugin needs an environment variable that isn't on the allowlist (for example `AWS_PROFILE` for an `entire-deploy` command), the user can opt names back in via `ENTIRE_PLUGIN_ENV`. It's a comma-separated list of either exact names or `PREFIX_*` wildcards:

```sh
# Forward AWS_* and EDITOR
ENTIRE_PLUGIN_ENV='AWS_*,EDITOR' entire deploy

# Forward a single token
ENTIRE_PLUGIN_ENV='GH_TOKEN' entire pgr
```

`ENTIRE_PLUGIN_ENV` itself is forwarded to plugins (it matches the `ENTIRE_` prefix), so plugins can introspect what was opened up.

### Why filter?

This is a **defense-in-depth** boundary, not a security perimeter. Plugins on `$PATH` are trusted to run as the user — they can read `~/.aws/credentials` directly if they want. The filter exists to:

1. Avoid accidental token leakage to plugins that don't need credentials.
2. Make the contract between the CLI and a plugin explicit (plugins document the env they require).
3. Catch typos and stale env (a forgotten `OPENAI_API_KEY=...` from yesterday's experiment).

Plugin authors who need a variable should either rely on the allowlist or document the `ENTIRE_PLUGIN_ENV` value users should set.

## Author Contract

External commands are arbitrary executables. No SDK, no protocol, no manifest. The contract:

- **Stdio is the parent's terminal.** Stdin, stdout, and stderr are connected directly. The command can prompt interactively, stream output, and behave like any other CLI tool.
- **Exit codes propagate verbatim.** The parent `entire` exits with the child's exit code.
- **Signals reach the child.** Terminal signals (Ctrl+C) reach the child directly via the foreground process group. If the parent's context is cancelled (e.g. via `signal.Notify` plumbing), the child receives `SIGINT` with a 5-second grace before the runtime falls back to `SIGKILL`. Commands that need clean shutdown should trap `SIGINT`.
- **Arguments after the command name pass through verbatim.** `entire pgr --help foo` invokes `entire-pgr` with argv `["--help", "foo"]`. Cobra's flag parsing does not run.
- **Windows.** On Windows, `exec.LookPath` resolves `.exe`, `.bat`, and `.cmd` extensions automatically. The "found but not executable" path is Unix-only — Windows treats extension match as the only correctness signal.

## What External Commands Do Not Get

- **No checkpoint integration.** File modifications are not tracked in checkpoints. External commands do not appear in `entire activity`. If a tool needs to participate in the session/checkpoint lifecycle, it must use the [agent protocol](external-agent-protocol.md) instead.
- **No transcript recording.** External-command stdio is not captured.
- **No hook installation.** External commands cannot register git hooks or agent hooks via the resolver. They are free to install their own, but `entire` does not coordinate.
- **No automatic update checks for the command itself.** The CLI runs `versioncheck.CheckAndNotify` for the parent CLI's version, not the child's. Authors should handle their own update notifications.

## Telemetry

External-command invocations are tracked only for names on a hardcoded allowlist (`officialPlugins` in `cmd/entire/cli/plugin_official.go`). Third-party command names are **never** sent — even with telemetry opted in. The reasoning matches gh's extension-telemetry posture: arbitrary command names can carry sensitive identifiers (project names, vendor names), and the safest default is silence.

When an allowlisted command runs successfully, the CLI emits a `cli_plugin_executed` event with:

- `plugin` — the command name
- `command` — `entire <name>`
- `cli_version`, `os`, `arch`, `isEntireEnabled`

Args and flags are deliberately **not** recorded.

Telemetry fires only when:

1. The command name is in `officialPlugins`.
2. `entire` settings have `Telemetry: true`.
3. `ENTIRE_TELEMETRY_OPTOUT` is unset.
4. The command exited with status 0. Failed/crashing invocations are not tracked, matching Cobra's `PersistentPostRun` semantics for built-in commands.

## Adding an Entire-Shipped Command to the Allowlist

When publishing an Entire-owned external command (e.g. `entire-pgr`):

1. Append the command name to `officialPlugins` in `cmd/entire/cli/plugin_official.go`.
2. Match must be exact and case-sensitive — the binary on disk is `entire-<name>`.
3. Update or add tests if the command has unusual telemetry shape.

Once allowlisted, `cli_plugin_executed` events for that command will flow through the existing PostHog pipeline.

## Comparison with the Agent Protocol

| | External Commands | [Agent Protocol](external-agent-protocol.md) |
|---|---|---|
| **Binary name pattern** | `entire-<name>` | `entire-agent-<name>` |
| **Discovery** | Lazy, on first non-built-in arg | Lazy at command entry, gated by `external_agents` setting (setup flows bypass the gate via `DiscoverAndRegisterAlways`) |
| **Communication** | Process exec; stdio passthrough | Subcommand protocol; JSON over stdin/stdout |
| **Versioning** | None | `ENTIRE_PROTOCOL_VERSION` envelope |
| **Lifecycle integration** | None | Full (sessions, checkpoints, hooks, transcripts) |
| **Telemetry** | Allowlist only | Standard agent telemetry |
| **Working directory** | User's cwd | Repository root |
| **Use when** | You want to add a CLI verb | You want an AI agent to participate in checkpointed sessions |

## Implementation

The resolver lives in `cmd/entire/cli/plugin.go`. The entry point is `MaybeRunPlugin(ctx, rootCmd, args)`, called from `cmd/entire/main.go` before `rootCmd.ExecuteContext`. Returns `(handled bool, exitCode int)` — when `handled` is true, the caller exits with `exitCode`; otherwise it falls through to normal Cobra execution.

Key files:

- `cmd/entire/cli/plugin.go` — entry point, `resolvePlugin`, `runPlugin`
- `cmd/entire/cli/plugin_env.go` — `pluginEnv`, the allowlist, and `ENTIRE_PLUGIN_ENV` parsing
- `cmd/entire/cli/plugin_official.go` — `officialPlugins` allowlist, `IsOfficialPlugin`
- `cmd/entire/cli/plugin_store.go` — managed install directory, `PluginBinDir`, `PluginDataDir`, `InstallPluginFromPath`, `ListInstalledPlugins`, `RemoveInstalledPlugin`, `PrependPluginBinDirToPATH`
- `cmd/entire/cli/plugin_manifest.go` — `pkg/<name>/manifest.yml` provenance records, `entire-plugin.yml` schema
- `cmd/entire/cli/plugin_gitremote.go` — `git ls-remote` tag resolution, blobless metadata fetch
- `cmd/entire/cli/plugin_fetch.go` — forge URL conventions, checksum verification, archive extraction
- `cmd/entire/cli/plugin_install_remote.go` — remote install/upgrade orchestration
- `cmd/entire/cli/plugin_index.go` — git-synced index cache, URL precedence
- `cmd/entire/cli/plugin_deps.go` — dependency planning, remove guard, `plugin doctor`
- `cmd/entire/cli/plugin_group.go` — `entire plugin install/list/remove/upgrade/search/info/browse/doctor/index` Cobra commands
- `cmd/entire/cli/telemetry/detached.go` — `BuildPluginEventPayload`, `TrackPluginDetached`
- `cmd/entire/cli/integration_test/external_command_test.go` — end-to-end coverage of the resolution path
- `cmd/entire/cli/integration_test/plugin_remote_install_test.go` — end-to-end remote install, dependencies, doctor
