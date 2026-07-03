# `entire ticket`

Experimental command for linking a repository branch to a tracker ticket
(Linear today) and carrying the ticket's intent into agent context, reviews, and
checkpoint provenance.

> Hidden while it matures — every subcommand still runs when invoked directly.

## Basic use

```sh
entire ticket setup            # pick a platform + team, store an API key securely
entire ticket start <id>       # make a branch, link the ticket, prep the prompt
entire ticket link [<id>]      # link a ticket to the current branch (any time)
entire ticket unlink           # remove the link from the current branch
entire ticket status           # show config, credential, linked ticket + drift
entire ticket revoke-token     # remove the stored credential
```

Useful flags:

```sh
entire ticket setup --platform linear --team ENG --token <key>   # non-interactive
entire ticket start ENG-142 --branch feature/eng-142             # name the branch
entire ticket start ENG-142 --no-branch                          # stay on current branch
entire ticket link ENG-142 --force                               # relink to a different ticket
entire ticket status --json                                      # machine-readable
```

`id` is auto-detected from the branch name when omitted. You can link a ticket
**before, during, or after** the work — the pointer is durable.

## Storage

A ticket link is a per-branch pointer; the ticket content is fetched live each
time. Three local stores plus one remote:

- `.entire/settings.json` — platform + team (per repo, committed).
- **OS keychain** — the API credential (via `tokenstore`; never in settings,
  logs, or git). Headless/CI can fall back with `ENTIRE_TOKEN_STORE=file`.
- `.git/entire-tickets/links.json` — branch → link + last-seen snapshot (in the
  git common dir, shared across worktrees).
- **Linear API** — the live ticket, reached only through the `Provider`
  interface so the surface stays tracker-agnostic.

## Behavior

- **Best-effort, never blocking.** Every remote step degrades gracefully — a
  fetch failure falls back to an id-only prompt (`start`) or stored state
  (`status`); the command still completes.
- **Live + snapshot.** Content is re-fetched on demand and compared against the
  last-seen snapshot, so `status` flags **drift** when the ticket changed
  mid-work.
- **Observe-only reads.** Once captured, the ticket surfaces (read-only) across
  the CLI — it never mutates the tracker:

  | Surface | Source | Shows |
  | --- | --- | --- |
  | `checkpoint explain` | frozen `Metadata.Ticket` | `ticket` header row + URL (`--json` gains a `ticket` object) |
  | `entire status` | live link (current branch) | a `Ticket ·` line under the branch (`--json` field) |
  | `entire session info` | live link (session's branch) | a `Ticket:` row (`--json` field) |
  | `entire review` | live link (current branch) | prepends the linked ticket as reviewer grounding |

  `explain` reads the *captured* snapshot (durable provenance); the other three
  read the *current* link. `session list` is excluded (one card per session).

- **Checkpoint capture.** On commit, the linked ticket is frozen into each
  committed checkpoint's `metadata.json` on `entire/checkpoints/v1`, so the
  checkpoint chain doubles as the ticket's timeline.

## Not built (deliberate)

- **Write-back** — `Comment` / `SetState` are implemented on the provider but
  unwired; no lifecycle event mutates the tracker yet. This is the next slice.
- **Agent launch on `start`** — prepared but off (cost); `start` prints the
  prompt instead.

## Key files

- `cmd/entire/cli/ticket/` — command group (`setup`, `link`, `start`, `status`,
  `revoke-token`), the `Provider` interface, the Linear client, link store, and
  snapshot/drift logic.
- `cmd/entire/cli/ticket_bridge.go` — deps bridge (avoids an import cycle).
- `cmd/entire/cli/ticket_display.go` — shared render helpers for the read
  surfaces (`formatTicketRefLine` / `formatTicketLinkLine` / `ticketBriefFromLink`).
- `api/checkpoint/metadata.go` — `TicketRef` frozen into checkpoint metadata.
- `settings.TicketConfig` · `internal/entireclient/tokenstore`.
