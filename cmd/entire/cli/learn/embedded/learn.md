## Set up & connect

Turns on Entire in this repository and wires it to your agents. Run `entire enable` once per repo, then `entire agent add` to install hooks for each agent you use.

- `entire enable` — Enable Entire with session tracking for your AI agent workflows
- `entire agent add` — Install hooks for an agent in this repository
- `entire agent list` — List installed and available agents
- `entire agent remove` — Uninstall hooks for an agent from this repository

## Observe current state

Tells you what's active right now — which session owns the current worktree, what all tracked sessions look like, and your cross-repo activity overview. Useful when you return to a repo after a break and need to orient quickly.

- `entire status` — Show whether Entire is currently enabled or disabled
- `entire session current` — Show the most recently active session for the current worktree
- `entire session list` — List all sessions, including ended ones
- `entire session info` — Display detailed state for a session
- `entire activity` — Show your activity overview, repository breakdown, and recent commits from entire.io

## Switch or resume

Switch branches and pick up an agent session exactly where it left off, or attach a session that ran outside the normal hook lifecycle. Useful when juggling multiple branches or pulling in a session that started before hooks were installed.

- `entire session resume` — Switch to a local branch and resume the agent session from its last commit
- `entire session attach` — Attach an existing agent session that wasn't captured by hooks
- `entire session stop` — Mark one or more active sessions as ended

## Find prior work

Search past checkpoints, commits, and sessions by topic, keyword, or natural language using hybrid semantic and keyword matching — useful when you can't remember which session fixed a bug or led to a refactor.

- `entire checkpoint search "<query>"` — Search checkpoints, commits, and sessions using semantic and keyword matching
- `entire checkpoint list` — List checkpoints on the current branch

## Understand a change

Shows you the original prompt, agent response, and files touched behind any checkpoint or commit, so you can tell what intent shaped the diff — yours or a teammate's.

- `entire checkpoint explain` — Explain a session, commit, or checkpoint in human-readable context

## Summarize & share

Generate a human-readable summary of recent agent work for standup, handoff, or your own weekly review.

- `entire recap` — Summarize recent checkpoint activity
- `entire dispatch` — Generate a dispatch summarizing recent agent work

## Troubleshoot

Detects and offers fixes for stuck sessions, broken metadata branches, or hook misconfiguration. Run `entire doctor` first whenever something feels off before digging deeper.

- `entire doctor` — Scan for session issues and offer to fix them

## Labs

`entire labs` is where commands live before they're ready for the main surface — you get early access to new workflows, with the understanding that invocations and behavior may shift. Try them freely; feedback shapes what graduates.

- `entire review` — Run configured review skills against the current branch
- `entire learn` — Learn the Entire CLI
- `entire investigate` — Run a multi-agent investigation against a topic, issue, or seed doc
- `entire org` — Manage Entire organizations (create, list)
- `entire project` — Manage Entire projects (create, list)
- `entire repo` — Manage Entire repositories (create, list, get, delete)
- `entire grant` — Manage access grants and org membership (org, project, repo)

## External agents

Entire ships with built-in support for several agents (run `entire agent list` to see them). For anything else, drop an `entire-agent-<name>` binary on your PATH and it shows up alongside the built-ins, ready for `entire agent add`.

  https://github.com/entireio/external-agents

## Skills

Entire publishes a curated library of agent skills — slash commands and integrations that drop into Claude Code, Codex, Cursor, OpenCode, and other supported agents.

  https://github.com/entireio/skills

## Other commands

- `entire auth` — Manage authentication (login, logout, status, and login-context management)
- `entire clean` — Clean up Entire session data
- `entire configure` — Update Entire settings in the current repository
- `entire disable` — Disable Entire in current repository
- `entire login` — Log in to Entire
- `entire logout` — Log out of Entire
- `entire plugin` — Manage Entire plugins (install, list, remove)
- `entire version` — Show build information

https://docs.entire.io/cli
