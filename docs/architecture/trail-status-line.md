# Trail Status Line

The trail status line surfaces the Entire trail connected to the current branch
inside an agent coding session, so the user always sees their trail (number,
title, open findings) and can click through to it without leaving the agent.

It is the native-CLI evolution of the `pi-trails-extension`: instead of
re-implementing Entire's auth and trail resolution in each agent's plugin
runtime, the CLI exposes one fast command that any agent integration polls.

## The polling engine: `entire trail status`

`entire trail status` (a subcommand of the hidden `trail` group, see
`trail_status.go`) prints a one-line summary of the current branch's trail.
It is built to be called on a hot path — every agent message — so it is fast
and never blocks:

- **Resolve**: origin remote → branch → authenticated data-API client → trail
  for the branch → a single page of open findings (counts only). This reuses
  the existing trail plumbing (`resolveTrailRemote`, `findTrailByBranch`,
  `NewAuthenticatedAPIClient`, `fetchTrailReviewComments` / `countTrailReviewComments`).
- **Cache**: results are cached per repo+branch under
  `<git-common-dir>/entire-sessions/trail-status.json` with a short TTL
  (`trailStatusFreshTTL`, 30s). A fresh entry is served with no network access.
- **Stale-while-revalidate**: on a cache miss the command refreshes within a
  short bound (`--timeout`, default 2.5s); if that fails it falls back to the
  stale entry rather than blanking the line.
- **Graceful degradation**: every "nothing to show" outcome (no trail, trails
  disabled for the repo, not logged in, unsupported repo, transient error)
  renders as empty output and exit 0, so a status line never shows an error or
  a stack of noise.

Output formats (`--format`):

| Format | Use | Notes |
|--------|-----|-------|
| `statusline` (default) | Claude Code status line | compact, ANSI-colored, OSC 8 hyperlink to the trail; empty for non-trail states |
| `plain` | humans / manual runs | informative for every state, no escapes |
| `json` | external readers | the full `trailStatusSnapshot` |

When given an agent status-line / hook JSON payload on stdin, the command reads
the workspace directory from it (`cwd` / `workspace.current_dir`) and resolves
the trail there, so it works regardless of the launch directory.

## Agent surfaces

### Claude Code — `statusLine` (persistent)

Claude Code renders a persistent status line configured in
`.claude/settings.json`. The `claudecode` agent implements the
`StatusLineSupport` capability (`agent/claudecode/statusline.go`) to manage it:

```json
{ "statusLine": { "type": "command", "command": "entire trail status" } }
```

Install is conservative — it only writes the entry when **no** status line
exists or the existing one is Entire's (upgrading the command if it changed). A
status line the user configured themselves is never clobbered. Uninstall
removes only Entire's entry.

### Codex and other hook agents — session-start banner

Codex has no persistent status line, only lifecycle hooks. The same trail
summary reaches it (and Cursor, Gemini CLI, …) through the existing
session-start banner: `handleLifecycleSessionStart` appends a one-line trail
summary via `sessionStartTrailBanner`. That helper is **cache-only** (no
network, so it never slows session start) and stays silent for agents that
already render Entire's status line, so the trail is shown in exactly one place
per agent.

## Enablement wiring

Installation rides the existing agent setup chokepoint, so every `entire enable`
path is covered:

- `setupAgentHooks` (`setup.go`) installs the status line for status-line-capable
  agents (`installAgentStatusLineBestEffort`) and warms the trail-status cache
  (`warmTrailStatusCacheBestEffort`) after installing hooks. Both are
  best-effort and bounded, so neither can fail or noticeably slow `enable`.
- The cache warm means the first status-line poll — and the cache-only Codex
  banner — render immediately after enable instead of cold.
- Removal mirrors hook removal: `uninstallAgentStatusLineBestEffort` runs at
  every uninstall site (`entire disable`, deselecting an agent on re-enable,
  `entire agent remove`, full `removeAgentHooks`).

The `StatusLineSupport` capability follows the established optional-interface
pattern (`agent.AsStatusLineSupport`, gated by `DeclaredCaps.StatusLine` for
external agents), so new agents with a status line opt in by implementing it.

## Key files

- `trail_status.go` — engine: resolve, cache, command.
- `trail_status_render.go` — statusline / plain / banner rendering, ANSI + OSC 8.
- `agent/agent.go`, `agent/capabilities.go` — `StatusLineSupport` + `AsStatusLineSupport`.
- `agent/claudecode/statusline.go` — Claude Code `.claude/settings.json` integration.
- `lifecycle.go` — session-start banner enrichment (Codex and other hook agents).
- `setup.go` — install/warm on enable, uninstall on disable.
