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

`entire plugin install/list/remove` manage the contents of this directory. Authors who prefer the raw "drop a binary on `$PATH`" model don't need to use it.

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
- `cmd/entire/cli/plugin_group.go` — `entire plugin install/list/remove` Cobra commands
- `cmd/entire/cli/telemetry/detached.go` — `BuildPluginEventPayload`, `TrackPluginDetached`
- `cmd/entire/cli/integration_test/external_command_test.go` — end-to-end coverage of the resolution path
