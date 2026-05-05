# Plugin System Plan: `entire plugin`

Status: Proposed
Last updated: 2026-05-04

A gh-style plugin system for the Entire CLI. Plugins are external executables
named `entire-<name>` that the CLI discovers and dispatches to when an unknown
subcommand is invoked. Modeled directly on the GitHub CLI extension system
(`gh extension`), with terminology adapted to "plugin" to leave room for
future lifecycle/event hooks.

## Background: how `gh extension` works

- **Naming + invocation.** Repos must be named `gh-<name>` and contain an
  executable of the same name. `gh <name> args...` falls through to
  `gh-<name> args...` with stdio inherited. Built-in commands cannot be
  overridden; `gh extension exec <name>` is the escape hatch.
- **Storage.** Per-user, under `~/.local/share/gh/extensions/gh-<name>/`. The
  manager scans this directory and classifies each entry as one of three kinds:
  - **Binary** — directory with a `manifest.yml` (`owner`, `name`, `host`,
    `tag`, `isPinned`, `path`). Installed by downloading the release asset
    matching the host's OS/arch.
  - **Git script** — git clone with an executable of the same name (no
    manifest). Updated via `git pull` / `git reset --hard origin/HEAD`.
  - **Local** — symlink, used by `gh extension install .` for development.
- **Dispatch.** Implemented in `pkg/cmd/extension/manager.go`. On Unix it
  `exec.Command`s the binary directly; on Windows it routes through `sh.exe`
  so shebangs work. Pinning is a `.pin-<sha>` marker file that blocks upgrade
  unless `--force`.
- **Conflict handling.** Install-time check walks the cobra tree
  (`rootCmd.Find()`) and refuses any name matching a built-in or alias. At
  runtime, cobra resolves built-ins first — a later-added built-in silently
  shadows an installed extension; `gh extension exec` is the only recovery.
- **Subcommands.** `install`, `create`, `list`, `upgrade`, `remove`, `exec`,
  `browse`, `search`. Update checks run every 24h (suppressible via env).
  Discovery is via the `gh-extension` GitHub topic; gh does no signing or
  verification and explicitly disclaims trust.

## Open decisions (defaults proposed)

1. **Trust model** — match gh's: no signing, no allowlist; install prints
   repo URL with first-time confirmation; `--yes` skips. Revisit if Entire
   ever ships an official registry.
2. **Plugin context** — argv passthrough plus a small set of env vars:
   `ENTIRE_REPO_ROOT`, `ENTIRE_SESSION_ID` (when active),
   `ENTIRE_PLUGIN_DATA_DIR`. No exposed Go SDK; plugins shell back into
   `entire` for privileged operations.
3. **Script plugins on Windows** — match gh: route through `sh.exe` for
   shebang support. Adds a small Windows-only code path; the alternative
   (Unix-only scripts) breaks parity.

## Package layout

```
cmd/entire/cli/plugin/
  manager.go            // discovery, classification, list, paths
  manager_test.go
  install_binary.go     // GitHub release asset → download → manifest
  install_git.go        // git clone path (script plugins)
  install_local.go      // symlink for `entire plugin install .`
  install_test.go
  upgrade.go            // binary: refetch release; git: pull/reset
  remove.go
  dispatch.go           // resolve `entire <unknown>` → exec
  dispatch_unix.go      // direct exec
  dispatch_windows.go   // sh.exe routing for script plugins
  dispatch_test.go
  manifest.go           // YAML schema (binary plugins only)
  http.go               // GitHub release fetching
  pin.go                // .pin-<sha> markers
  create/
    create.go           // scaffold subcommand
    templates/
      go/               // binary plugin scaffold + GH Actions release workflow
      bash/             // script plugin scaffold
  testutil/             // fake release server, fake git repo, fake plugin binaries
cmd/entire/cli/plugin_group.go   // cobra wiring: `entire plugin {…}`
cmd/entire/cli/plugin_exec.go    // `entire plugin exec <name>`
```

Naming follows the existing `<noun>_group.go` / `<noun>_<verb>.go`
convention from CLAUDE.md.

## CLI surface

```
entire plugin install <repo>     # owner/name, full URL, or "." for local
                                 # auto-detects: release assets → binary; else git clone
entire plugin install --pin <ver> <repo>
entire plugin list               # name, version, kind (binary|script|local), pinned?
entire plugin upgrade [<name>]   # all if omitted; --force overrides pin
entire plugin remove <name>
entire plugin exec <name> [...]  # bypass cobra (escape hatch for collisions)
entire plugin search <query>     # GitHub topic search: "entire-plugin"
entire plugin browse <name>      # open repo in browser
entire plugin pin <name> <ver>
entire plugin create <name> [--precompiled=go|other]
                                 # default: bash script template
                                 # --precompiled=go: Go binary + release workflow
                                 # --precompiled=other: language-agnostic binary scaffold
```

## Dispatcher wiring

Hook in `main.go`'s existing unknown-command branch (`main.go:42-47`).
Replace the `showSuggestion` call site with: try
`plugin.Dispatch(rootCmd, os.Args[1:])` first; on miss, fall through to
`showSuggestion`. Cobra resolves built-ins first → plugins can never shadow
built-ins. Conflict check at install time uses `rootCmd.Find()` and rejects
collisions with built-ins or aliases.

`Dispatch` returns `(handled bool, err error)`; `handled=false` falls through
to the suggestion path. Keeps the diff in `main.go` minimal and the contract
explicit.

Per-kind exec:

- **Binary / local** — direct `exec.Command` on Unix and Windows.
- **Script (git-cloned)** — direct exec on Unix (shebang honored). On
  Windows, route through `sh.exe` (gh's approach); document the dependency.

## Storage layout

```
$XDG_DATA_HOME/entire/plugins/        # ~/.local/share/entire/plugins; ENTIRE_PLUGIN_DIR override
  entire-foo/                         # binary plugin
    manifest.yml                      # owner, name, host, tag, isPinned, path
    entire-foo
    .pin-<sha>                        # optional
  entire-bar/                         # script plugin (git-cloned)
    .git/
    entire-bar                        # executable matching dir name
    README.md ...                     # repo contents
  entire-baz                          # local plugin (symlink → /path/to/dev/dir)
```

Classification on scan: symlink → local; has `manifest.yml` → binary;
otherwise → script.

## Manifest (binary plugins only)

```yaml
owner: octocat
name: foo
host: github.com
tag: v1.2.3
isPinned: false
path: /home/u/.local/share/entire/plugins/entire-foo/entire-foo
```

Field-for-field parity with gh's `binManifest` so intuition transfers.

## Install resolution

`entire plugin install <repo>`:

1. If arg is `.` → local symlink path.
2. Resolve `<repo>` to GitHub. Fetch latest release.
3. If release has an asset matching `entire-<name>_<os>_<arch>(.exe)?` →
   binary install.
4. Else → git-clone install (script plugin). Verify executable of same name
   as the directory exists and is `+x`.
5. Conflict check: `rootCmd.Find([]string{name})` must return root or the
   extension group; otherwise refuse with an error pointing at
   `entire plugin exec`.
6. Atomic placement: download/clone to `entire-<name>.tmp`, rename to final.

## Upgrade

- **Binary** — refetch latest release, replace binary atomically, rewrite
  manifest.
- **Script** — `git -C <dir> pull --ff-only` (or
  `git reset --hard origin/HEAD` with `--force`).
- **Local** — no-op (warn).
- **Pinned** — skip unless `--force`.

## Milestones (independently mergeable)

1. **Skeleton.** Storage, manager, classification, `list`, `remove`. Tests
   with hand-placed fake plugins of all three kinds.
2. **Dispatch.** All three kinds, Unix + Windows. Argv passthrough, exit-code
   propagation, stdio inheritance, `ENTIRE_*` env injection. Tests via
   `execx.NonInteractive` against fake plugins.
3. **Install — binary path.** GitHub release fetch with a mocked `httptest`
   server. Conflict check against `rootCmd.Find()`. Atomic swap.
4. **Install — git path + local.** Shell out to the `git` CLI (consistent
   with Entire's existing carve-out for go-git v5 quirks). `.` → symlink.
5. **Upgrade.** Both kinds, `--force` flag, pin handling.
6. **`entire plugin exec`.** Tiny; routes through dispatcher with built-in
   precedence bypassed.
7. **`entire plugin create`.** `bash` (default) and `go` (with GitHub Actions
   release workflow) templates under `create/templates/`. Embedded via
   `embed.FS`. `--precompiled=other` writes a minimal language-agnostic
   scaffold.
8. **Search + browse + pin.** Topic-based discovery via the `entire-plugin`
   GitHub topic.
9. **Update notifier.** 24h check, suppressible via
   `ENTIRE_NO_PLUGIN_UPDATE_CHECK`. Reuses `versioncheck` plumbing from
   `root.go:70`.
10. **Docs.** `docs/architecture/plugin-system.md`, "writing a plugin" page,
    CLAUDE.md update.

## Testing strategy

- `t.Parallel()` everywhere; isolation via `t.TempDir()` plus
  `ENTIRE_PLUGIN_DIR`.
- Spawn fake plugins via `execx.NonInteractive` (per CLAUDE.md guidance for
  spawning real binaries in tests).
- Mock GitHub release server with `httptest`.
- Git-path install tests use a local bare repo as the "remote" — no network.
- E2E: a Vogon-style fake plugin in `e2e/` covering install (binary + git) →
  list → exec → upgrade → remove. Add to the `test:ci` canary.
- Conflict-shadowing regression: register a built-in `foo`, install
  `entire-foo`, assert install fails; force-place a binary, assert
  `entire foo` resolves to the built-in and `entire plugin exec foo`
  resolves to the plugin.
- Windows script-dispatch test: gated build tag; verifies `sh.exe` routing.

## Out of scope for v1 (deferred deliberately)

- **Lifecycle/event hooks** (PostCommit, session-start, etc.) — phase 2;
  the door is left open by choosing "plugin" over "extension" as the name.
- **Signing / verification** — gh does not do this either.
- **Cross-machine sync** — gh does not do this either.
- **Central registry beyond GitHub topic search** — gh does not do this
  either.

## References

- gh extension manual: https://cli.github.com/manual/gh_extension
- Using GitHub CLI extensions:
  https://docs.github.com/en/github-cli/github-cli/using-github-cli-extensions
- gh extension source: https://github.com/cli/cli/tree/trunk/pkg/cmd/extension
- Naming conflict bypass discussion: https://github.com/cli/cli/issues/5427
