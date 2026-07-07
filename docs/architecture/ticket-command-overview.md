# `entire ticket` — Overview

*A plain-language companion to the [design spec](./ticket-command.md), for
reviewers and engineering managers. The spec has the low-level detail; this is
the "what, why, and where it's going."*

---

## TL;DR

`entire ticket` connects a repository to a ticket tracker (Linear today) so the
work an engineer does is grounded in the ticket's real requirements — and so
Entire captures those requirements as first-class context.

**Status:** the command surface is built, tested, and behind a hidden flag while
it matures. Agent launch is intentionally off (cost). Write-back to the tracker
and deep provenance capture are the next phases.

---

## Why we're building it

An AI agent is only as good as the intent it's given — and that intent lives in
the tracker, not the code. Today it's copy-pasted by hand or lost. Capturing it
unlocks a ladder of value:

1. **Now — better context.** Every task starts from its actual requirements.
2. **Next — straight-through delivery.** A ticket becomes a branch, a change, a
   reviewed PR, and a status update, with a human approving the result.
3. **Later — leverage.** Many tickets progressed in parallel ("streaming
   engineering hours"), and the captured *requirement → change → outcome* data
   becomes exactly what makes future models better.

This fits Entire's core thesis: as writing code becomes a commodity, the value
moves to the **edges** — getting the task *right* going in, and *trusting and
tracing* it coming out. `entire ticket` owns the "going in" edge and sets up the
"coming out" edge.

## What it does today

| Command | What it does |
|---|---|
| `entire ticket setup` | pick a platform + team, store an API key securely |
| `entire ticket status` | show config, credential, and the linked ticket (live) |
| `entire ticket link <id>` | attach a ticket to the current branch (any time) |
| `entire ticket unlink` | detach it |
| `entire ticket start <id>` | make a branch, link the ticket, prepare the agent prompt |
| `entire ticket revoke-token` | remove the stored credential |

**A typical flow:**

```
entire ticket setup                 # once per repo
entire ticket start ENG-142         # → new branch "eng-142", ticket linked,
                                    #   prompt built from the ticket's details
entire ticket status                # → shows the ticket + flags if it changed
```

You can link a ticket **before, during, or after** doing the work — the pointer
is stored per branch, so it works even if you forgot to link up front.

## How it works (at a glance)

```
  you ──▶ entire ticket ──┬─▶ settings.json   (platform + team)
                          ├─▶ OS keychain     (the API key — never in git)
                          ├─▶ .git/…          (branch → ticket link + snapshot)
                          └─▶ Linear API      (fetch the ticket, live)
```

- The **link** is just a pointer (branch → ticket). The ticket's *content* is
  fetched **live** each time, so it's always current.
- A **snapshot** of the last-seen ticket is kept so we can tell you when the
  ticket **changed mid-work** — you update to the new version, and get a
  heads-up in case a scope change invalidated code you already wrote.
- It's **tracker-agnostic** behind one interface; Linear is the first provider,
  others (Jira, GitHub) are adapters.

## Status: done vs. deferred

**Done:** all six commands, the Linear integration, secure credential storage,
live fetch, snapshot + change-detection, `--json` output, full test coverage.

**Deferred (on purpose):**
- **Agent launch** — a full agent per `start` is expensive; `start` prepares
  everything and prints the prompt instead.
- **Write-back** — posting results and moving the ticket (the API calls exist,
  they're just not wired to a command yet).
- **Deep provenance** — freezing the ticket into Entire's checkpoint history so
  reviews and `why` can use it. This is the phase that makes linking *durable*.

## Key decisions (and why)

- **Local-first.** Config and links live in the repo, not a backend — it works
  offline with no server dependency. Trade-off: not shared across machines yet.
- **Snapshot + always-update.** We keep the working copy current *and* detect
  change, rather than a full version history (the tracker already has that).
- **Personal API key first.** Simple to set up; the richer "Entire-as-an-agent"
  Linear integration comes later.
- **Reuses Entire's patterns, doesn't reinvent.** New code is limited to what's
  genuinely ticket-specific; everything else follows existing conventions.

## Risks & how we handle them

| Risk | Mitigation |
|---|---|
| **Credential leakage** | stored in the OS keychain, never in settings/logs/git; `revoke-token` + rotation |
| **Untrusted ticket text** | ticket bodies are attacker-influenceable; launch is off today, and will adopt the same guardrails as issue-seeded investigations when it returns |
| **Cost** | agent launch deferred; fetch/keychain costs are negligible |
| **Tracker/network down** | best-effort: commands still complete with a fallback |
| **Headless Linux (no keychain)** | falls back to a file-backed store via env var |

## Roadmap

1. **Ship the command surface** *(done)* — link, context, status.
2. **Capture into checkpoint context** *(next)* — makes linking durable
   provenance; gives ticket "versioning" for free via the checkpoint timeline.
3. **Write-back** — post the review verdict, move the ticket to In Review / Done.
4. **More providers** — Jira, GitHub Issues, etc.
5. **Linear Agent Sessions** — Entire appears as a first-class agent in Linear.
6. **Re-enable agent launch** behind cost controls.

## Engineering notes

- Shipped as **13 commits across a stack of small PRs** (one per capability),
  each independently building, testing, and passing lint.
- **Tested hermetically** — no network or real keychain in unit tests; the real
  keychain is verified by running the binary (with `revoke-token` for cleanup).
- **Convention-aligned** — reuses the noun-group + deps-bridge shape, the
  settings/token/UI/git helpers, and the error/output conventions already in the
  codebase.

*For interfaces, data models, failure cases, and security detail, see the
[full design spec](./ticket-command.md).*
