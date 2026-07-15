# Lua Plugins

Entire supports two kinds of third-party plugins:

1. **Binary plugins** — kubectl-style `entire-<name>` executables on `$PATH` (see
   [External Commands](external-commands.md)). Any language, but require a build
   and a release pipeline.
2. **Lua plugins** — no-build-step scripts that subscribe to lifecycle/git hooks
   and contribute commands, run inside an embedded, sandboxed Lua interpreter.

This document covers Lua plugins. They are layered on top of the binary plugin
mechanism without replacing it: both are discovered and dispatched, with a
defined precedence.

The runtime is pure Go ([`github.com/yuin/gopher-lua`](https://github.com/yuin/gopher-lua)),
so it builds with `CGO_ENABLED=0` on every target — no native Lua dependency.

## Trust model (read this first)

Lua plugins execute arbitrary code in your `entire` process. The security model
is **opt-in at every step**:

- A plugin is **inert until allow-listed**. Discovery finds plugins in the
  managed user dir and in the repo-local `.entire/plugins/`, but none run unless
  there is an entry for it under `plugins` with `"enabled": true`.
- **Repo-local plugins can only be enabled from your personal, uncommitted
  `.entire/settings.local.json`** — never from the committed team
  `.entire/settings.json`. A repo can ship a `.entire/plugins/foo` directory
  *and* a team `.entire/settings.json` with `plugins.foo.enabled = true`, but
  that team entry does **not** activate a repo-local plugin. It stays inert until
  *you* add `plugins.foo.enabled = true` (and any capabilities) to your own
  `.entire/settings.local.json`. This is what makes cloning a hostile repo safe:
  a malicious PR cannot ship both the plugin and the enable and have it run on
  clone. (User-global installed plugins under the managed `lua/` dir are
  unaffected — they may be enabled from either file.)
- **Privileged APIs are capability-gated.** Network, subprocess, filesystem, and
  the mutating hooks are denied unless the capability is granted in the plugin's
  allow-list entry. An ungranted call raises a Lua error (fail loud, never a
  silent no-op).
- **Installing ≠ running.** `entire plugin install <git-url>` only places files.
  The cloned code cannot run until you complete the allow-list step above.
- **Kill switch.** `ENTIRE_PLUGINS_DISABLED=1` disables all plugins process-wide
  regardless of settings.

The allow-list plus capability grants are the trust boundary. The sandbox
(curated stdlib, stripped escape hatches, execution timeouts) limits *accidental*
damage and blast radius; it does not claim to fully contain a deliberately
malicious plugin you have explicitly enabled. Treat enabling a plugin like
running its code — because that is what it is.

## Layout

Managed (installed) Lua plugins live one directory per plugin under the managed
plugin tree, a sibling of the binary store's `bin/` and `data/` dirs:

```
~/.local/share/entire/plugins/lua/<name>/    # installed Lua plugins
  plugin.json
  main.lua
~/.local/share/entire/plugins/data/<name>/   # per-plugin durable kv storage
.entire/plugins/<name>/                       # repo-local (never auto-runs)
```

The parent dir honors `ENTIRE_PLUGIN_DIR` (absolute override), then
`XDG_DATA_HOME` on Unix / `LOCALAPPDATA` on Windows, then the platform default —
identical resolution to the binary store.

## Manifest (`plugin.json`)

Parsed strictly: unknown keys are rejected so typos surface at load time.

```json
{
  "name": "checkpoint-notify",
  "version": "1.0.0",
  "description": "Desktop notification on each checkpoint",
  "entry": "main.lua",
  "hooks": ["checkpoint_saved", "post_commit"],
  "commands": [{ "name": "notify-test", "short": "send a test notification" }],
  "capabilities": ["exec"]
}
```

| Field          | Meaning                                                             |
| -------------- | ------------------------------------------------------------------ |
| `name`         | Required. Dispatch-safe identifier (keys the allow-list/data dir). |
| `version`      | Informational.                                                     |
| `description`  | Shown by `entire plugin list`.                                     |
| `entry`        | Entry script, default `main.lua`. Bare file name in the dir.       |
| `hooks`        | Declared hooks (validated). Actual subscription is via `entire.on`. |
| `commands`     | Declared commands. Actual registration is via `entire.command`.    |
| `capabilities` | Requested capabilities (only take effect when also granted).       |

The manifest documents intent; the settings allow-list grants power.

## Settings allow-list

Mirrors the `external_agents` opt-in posture.

```json
{
  "plugins": {
    "checkpoint-notify": { "enabled": true, "capabilities": ["exec"] },
    "models-updater":    { "enabled": true, "capabilities": ["http", "fs"] }
  }
}
```

- `enabled` — the plugin runs only when `true`.
- `capabilities` — granted capabilities. Unknown names are rejected at load.
  Valid: `http`, `exec`, `fs`, `net`, `commit_msg`, `pre_push`.

Team settings (`.entire/settings.json`) and per-developer overrides
(`.entire/settings.local.json`) merge per-plugin: an override can enable or grant
extra capabilities without hiding the team's entries.

**Scope matters for repo-local plugins.** Because `.entire/settings.json` is
committed, a hostile repo could otherwise ship both a `.entire/plugins/<name>`
directory and a team entry enabling it. To prevent that, a **repo-local plugin
is governed only by `.entire/settings.local.json`** (which is not committed): its
`enabled` flag and capabilities must come from that personal file, and any entry
for it in the committed team `.entire/settings.json` is ignored for activation.
User-global plugins installed under the managed `lua/` dir are governed by the
merged allow-list and may be enabled from either file.

## The `entire` Lua API

The entry script runs once at load and registers hooks/commands. See the
annotated stub in [`examples/plugins/entire.lua`](../../examples/plugins/entire.lua)
for editor autocompletion.

### Always available

- `entire.on(hook, function(event) ... end)` — subscribe to a hook.
- `entire.command{ name=, short=, run=function(args) ... end }` — contribute a
  CLI command (`entire <name>`); `run` returns an integer exit code (nil → 0).
- `entire.log.debug|info|warn|error(msg)` — write to the Entire log.
- `entire.kv.get(key) / set(key, value) / delete(key)` — durable per-plugin
  string store (JSON file in the data dir).
- `entire.print(...) / entire.write(...)` — write to stdout (for commands; avoid
  in hooks, where it can corrupt hook stdout). The global `print()` is routed to
  the log instead.
- Read-only accessors: `entire.plugin_name`, `entire.version`, `entire.source`
  (`"user"` or `"repo"`), `entire.repo_root`, `entire.data_dir`.

### Capability-gated

Each raises a Lua error naming the missing capability when ungranted.

- `entire.http.get(url) / post(url, body[, content_type])` → `{status, body}`
  (`http`; http/https only, 10s timeout, 5 MiB response cap).
- `entire.exec.run(cmd, arg1, ...)` → `{stdout, stderr, code}` (`exec`; 30s
  timeout, runs in the repo root).
- `entire.fs.read(path) / write(path, contents)` (`fs`; confined to the repo
  root and the plugin data dir — traversal outside is rejected).
- `entire.net.connect(...)` (`net`; reserved, not implemented — use `entire.http`).

## Hooks

Observer hooks run for side effects only; a failing or slow observer is logged
and ignored, never propagated. Each callback runs under a per-hook timeout
(2s default, `ENTIRE_PLUGIN_HOOK_TIMEOUT_MS` to raise) with panic isolation.

| Hook               | Fires                                             | Kind     |
| ------------------ | ------------------------------------------------- | -------- |
| `session_start`    | agent session begins                              | observer |
| `turn_start`       | user submits a prompt                             | observer |
| `turn_end`         | agent finishes a turn                             | observer |
| `checkpoint_saved` | a session step checkpoint is written              | observer |
| `post_commit`      | a git commit is processed                         | observer |
| `pre_push`         | before a git push                                 | observer + veto |
| `subagent_end`     | a subagent/task completes                         | observer |
| `session_end`      | a session ends                                    | observer |
| `compaction`       | the agent compacts its context                    | observer |
| `model_update`     | the agent reports the active model                | observer |
| `prepare_commit_msg` | building a commit message                       | mutating |

Event payloads carry operational metadata (ids, agent, model, file paths) — not
prompt text or file contents. Richer/sensitive data is reserved for
capability-gated APIs.

### Mutating hooks

- `prepare_commit_msg` (capability `commit_msg`): the callback may return a
  trailer string appended to the commit message. Plugin trailers land **after**
  the built-in `Entire-Checkpoint` trailer (never displacing it) and before the
  git comment block. Multiple plugins contribute in load order.
- `pre_push` (capability `pre_push`): the callback may return `false` (with an
  optional reason string) to veto the push, aborting it with a non-zero pre-push
  hook exit. The veto runs **before** the built-in OPF rewrite and
  checkpoint-ref push so it short-circuits that work. Plugins without the
  capability still receive the observer fire; their return value is ignored.

## Command resolution order

`entire <name>` resolves in this order:

1. **Built-in** Cobra command (always wins).
2. **Lua plugin command** (`entire.command`).
3. **`entire-<name>` binary plugin** (kubectl-style).

Lua commands are dispatched before the binary dispatcher, so a Lua command wins
over a same-named binary plugin; both defer to built-ins. Plugins are only loaded
when the first arg is a plugin-shaped, non-built-in name, so built-in commands
pay no discovery cost.

## Distribution

```bash
# From a git URL (cloned into the managed lua dir; --ref pins a tag/branch/commit)
entire plugin install https://github.com/acme/entire-notify.git --ref v1.2.0

# From a local directory containing a plugin.json
entire plugin install ./my-plugin

# Update git-installed Lua plugins
entire plugin update            # all
entire plugin update notify     # one

# List (Lua + binary) / remove
entire plugin list
entire plugin remove notify
```

Install only places files — remember to allow-list the plugin in settings before
it will run.

## Implementation

- `cmd/entire/cli/plugins/` — the runtime: sandbox, manifest, loader/registry,
  hook bus (observer + mutating), Lua API, capability enforcement, commands.
- `cmd/entire/cli/settings/settings.go` — `PluginSettings` allow-list + capability
  validation.
- `cmd/entire/cli/lua_plugin_hooks.go`, `strategy/plugin_hooks.go` — the seams
  that fire hooks from lifecycle events and git hooks.
- `cmd/entire/cli/plugin_lua_command.go` — command dispatch.
- `cmd/entire/cli/plugin_lua_store.go`, `plugin_group.go` — install/update/list/remove.

The `plugins` package deliberately does not import `cli` or `strategy` (which
import it); seams build plain `map[string]any` payloads that the package converts
to Lua tables.
