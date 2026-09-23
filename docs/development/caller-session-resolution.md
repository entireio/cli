# Caller-session resolution

Identity ranking, provenance, and known limitations of caller-session selection. Read before changing session current/tokens/adopt or caller identification.

Repository paths in code spans are relative to the repository root unless stated otherwise.

### Resolving the calling session

`strategy.ResolveCallerSession` answers "which session is running this
process?", degrading to "which session is current here?" only when nothing can
identify the caller. Tiers, strongest first, each reported back as
`SessionResolution`:

1. **Identification** — the environment and process ancestry ranked
   *together*, reporting `caller-env`, `ancestry`, or `caller-ambiguous`.
2. **`worktree`** — the most recently active session recorded in this worktree.
3. **`other-worktree`** — this worktree has no sessions at all, so the most
   recent one from anywhere in the shared store.

**Environment and ancestry are one tier, and a nearer owner outranks an
environment claim.** They were two tiers, environment first, returning on any
hit — which is wrong for nesting: an inner agent that publishes no ID of its
own (Gemini CLI, opencode) forwards the OUTER agent's variable straight
through, so the only claim named the outer session while the inner one sat one
hop away in our ancestry. The resolver reported the outer session as
`caller-env` and `IsCaller()` true — "safe to act on" — which is exactly the
mistake the type exists to prevent. Depth is the only signal that separates
"Codex ran me" from "Codex ran Gemini ran me", so the nearest owner wins
wherever ancestry can rank at all. The environment's remaining job is real and
narrower: naming a session ancestry *cannot* rank — one whose owner was never
recorded (no turn yet), or any session on a platform that cannot introspect
processes.

`session tokens` preserves this provenance in its JSON `resolution` field and
in text/agent-brief `Resolved:` lines (omitted for an explicit session ID).
Ambiguous matches warn on stderr before recommendations are printed. Untracked
callers reuse `session current`'s diagnostic; JSON and agent-brief modes leave
stdout empty and exit non-zero rather than emitting a token report.

**`caller-ambiguous` is a third outcome of that tier, and it does not satisfy
`IsCaller()`.** The rule (`claimsRuledOut`) is that **the winner is believed
only when no session the environment named could be nearer to us than it is.**
A claim reached our environment, so its agent is certainly somewhere in our
ancestry; what is unknown is where. A claim we could not place — untracked, so
no owner to compare, or tracked with no owner recorded yet — therefore sits at
an unmeasured depth, and unmeasured means possibly nearer. Exactly two
exemptions: a winner at depth 0 owns our immediate parent, so nothing can be
nearer; and a claim that IS the winner shadows nothing, which is the ordinary
single-agent shape.

Three revisions of this rule were wrong in review, each in the same direction —
overclaiming identification — and the sequence is worth knowing because the
next attempt will be tempted by the same shortcut:

1. Environment first, returning on any hit. Wrong for nesting: the lone claim
   named the *outer* session while the inner one sat a hop away in ancestry.
2. Ambiguous only when several claims went unplaced. Missed the mixed case:
   only sessions with state enter the ranking, so a tracked outer session won
   by default while an untracked inner claim was never examined.
3. Exempting "exactly one claim", on the reasoning that a lone claim has
   nothing to be nearer than. True of a lone claim that *wins* — and the
   revision assumed those were the same thing. A claim with no state cannot
   enter the ranking at all, so a session found purely by ancestry won instead
   and was reported as identified while the claim naming itself in our own
   environment went unexamined.

**Do not reintroduce a count-based exemption.** The question is about the
winner, not about how many claims exist. The reported session is still the most
useful of the candidates (a tracked one over an ID Entire knows nothing about);
what the resolution says is whether it was identified or guessed.

**The command must not narrate a guess as a fact either.** `session current`'s
untracked diagnostic used to say "This command is running inside X session Y",
which states an arbitrary pick as the answer when several untracked claims
exist. It now says which sessions claim it and that the caller could not be
determined; the diagnosis survives, the identification does not.

**Tier 3 is why this exists.** `entire session current` used to collapse tiers
2 and 3 and describe either as "the active session for the current worktree".
Worktrees share one session store, so in a worktree with no sessions of its own
it returned a live session belonging to a *different* worktree —
indistinguishably from a real answer, with that worktree's path in the JSON.
Agents read the ID back and fed it to `entire session adopt`, which moves the
named session into the current worktree and resets its checkpoint bookkeeping:
a wrong ID there mutates a third party's running session. The tier still
exists, because "what has been happening in this repo" is a real question — it
just has to say that is what it answered. `SessionResolution.IsCaller()` is the
gate for anything that *acts* on a session rather than displaying it; only
tier 1 passes, and then only when it resolves to `caller-env` or `ancestry`.

**`IsCaller()` currently guards nothing, and that is the open half of this
work.** `session adopt` — the command whose damage motivated the tiering, since
it moves a session and resets its checkpoint bookkeeping — does not consult it.
Its own checks are narrower than a most-recent guess (an explicit `--from`, and
auto-selection scoped to that worktree's recent adoptable sessions) but none of
them asks "is this session mine": `sessionBelongsToSourceWorktree` only checks
that the ID and the worktree agree with each other, which the weak tiers'
output satisfies by construction. Reaching it does not even need `session
current`, since `adopt --from <path>` with one recent session there
auto-adopts. Wiring the guard is a separate change with its own question to
settle — what "mine" means for a `--from` on another machine, where ancestry
cannot apply.

**Tier 1 is per-agent and declarative.** An agent implements
`agent.CallerSessionIdentifier` by naming the variable it publishes
(`CallerSessionEnvVar`), and the `agent` package does the reading and
validation — so the ID is checked with `validation.ValidateAgentSessionID` in
one place (it becomes a path component in `ResolveSessionFile`, so an
unvalidated one is a traversal sink), and `agent.CallerSessionEnvVars()` can
enumerate the set. Five agents publish one: Claude Code
(`CLAUDE_CODE_SESSION_ID`), Codex (`CODEX_SESSION_ID` — the root-session
identity, *not* `CODEX_THREAD_ID`, which follows forks and subagent threads),
Cursor (`CURSOR_CONVERSATION_ID`), Copilot CLI
(`COPILOT_AGENT_SESSION_ID`), and pi (`PI_SESSION_ID`). Each was established
against the shipped agent rather than inferred, and each resolves to the same
ID that agent's lifecycle events report — so no translation is needed.

These names are stated in four places (here, each agent's
`CallerSessionEnvVar`, the static `agent.callerSessionEnvVars`, and
`callerSessionEnvVarByAgent` in `agent/caller_session_test.go`); **the test
table is the enforced copy**, so trust it if they ever diverge, and
`TestCallerSessionEnvVars_MatchesTheRegistry` pins the static list against the
live registry.

**`CallerSessionEnvVars()` is static rather than registry-derived on purpose**,
which looks backwards until you see the failure: its consumers are the test
harnesses that isolate themselves from the developer's real agent session, and
not every test binary links every agent implementation — the e2e harness links
eight of the nine, omitting pi. A registry-derived list silently shortens to
that binary's subset, and a missing name is not an error, it is one variable
left set, so a real session leaks into the run and surfaces as an unrelated
assertion failure on one machine. Registration cannot be the source of truth
for "every name that exists". Worth knowing because a wrong name fails
silently — it degrades to a weaker tier rather than erroring — and the guard
test catches a newly capable agent going unlisted, not a vendor renaming a
variable we already track.

Gemini CLI and opencode publish **nothing**, and that is a finding rather than
a gap in our table: Gemini passes its session ID to its shell executor for
background-process bookkeeping but never into the child environment, and
opencode's shell tool performs no environment augmentation at all. Tier 2 is
what covers them, which is why it is not optional.
`TestCallerSessionEnvVar_UnpublishedAgentsStayUnpublished` fails if either
gains the capability without its variable being pinned.

Two states worth distinguishing, both on tier 1:
`ResolvedSession.Tracked == false` means the agent named a session Entire holds
no state for — hooks are not installed, they failed, or the first turn has not
landed (state is created at turn start). That is a diagnosis, so the ID and
agent are reported; falling through to a weaker tier would answer a question
nobody asked with someone else's session.

Several tier-1 claims at once is the normal **nested** case, not a conflict: a
`codex exec` run from Claude Code's shell tool inherits the outer agent's
variables through the inner agent's process. Tracked claims are ranked by
ancestry depth (nearest wins), then by most recent interaction when ancestry
cannot separate them.

**Tests that touch this must clear the variables**, derived from
`agent.CallerSessionEnvVars()` rather than hand-listed. `go test` is routinely
run from inside one of these agents, so a leaked variable makes fixture-based
assertions pass on CI and fail on a contributor's machine.
