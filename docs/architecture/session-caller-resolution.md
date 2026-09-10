# Resolving the Calling Session

How Entire answers "which session is running this command?", why the weaker
answers are reported rather than hidden, and why `entire session adopt` is
gated on the strong ones.

Read this before changing `strategy.ResolveCallerSession`, the
`agent.CallerSessionIdentifier` capability, `findSessionsForCommitLinking`, or
`ensureAdoptSourceIsCaller`. It records decisions that were each arrived at by
getting them wrong first — the wrong versions are kept deliberately, because
every one of them looks like the obvious simplification.

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

**`session adopt` is what `IsCaller()` guards**, via
`ensureAdoptSourceIsCaller`. That command is why the tiering exists: it moves
the named session and resets its checkpoint bookkeeping, so a wrong ID mutates
a third party's *running* session and detaches their next commit from it.

None of the pre-existing checks could catch that, and it is worth knowing why
rather than assuming they were merely incomplete.
`sessionBelongsToSourceWorktree` asks only whether the ID and the worktree
agree with *each other* — which any pair read out of `session current`
satisfies by construction, since they were reported together.
`isAdoptableSourceSession` asks only whether the session is still live, and
being live is precisely what makes hijacking it harmful. Neither asks "is this
mine".

Three outcomes: a session the resolver confirms is the caller's own is adopted
**silently**, because that is the case the command exists for (an agent whose
work moved to another worktree adopting its own session). A session
positively identified as someone else's, or one whose ownership cannot be
confirmed at all, is refused — a terminal gets a confirmation prompt, and
without one the command fails. Auto-selection is covered too, which matters
because `adopt --from <path>` needs no session ID at all and was the shape
reachable without `session current`.

**The ownership decision is taken twice, and the second time is the one that
authorizes.** The first runs on an unlocked snapshot, so it can only decide
whether to *ask*; `revalidateAdoptOwnership` retakes it against the state as it
exists under the lock, immediately before the mutation. Both mutation paths
already reloaded and re-checked every other precondition there — adoptability,
worktree membership, transcript ownership — and ownership was the one
precondition left outside that pattern, on a window that is not stable: a turn
start re-records `SessionState.Owner` (`captureSessionOwner`), and a session
created in the source store meanwhile can be a nearer owner than the one the
adoption was authorized against.

The re-check **fails rather than asks**. An override — the flag, or a human who
already answered — is honoured without re-checking, because that answer was
about this session and re-prompting mid-mutation would ask the same person the
same question twice. That is what `ensureAdoptSourceIsCaller` returns, and why
the ownership *decision* (`adoptSourceIsOwned`) is split from the *interaction*:
a decision with no side effects can be taken twice, a prompt cannot.

**No terminal means refuse, not proceed.** The reported incident was an agent
running this non-interactively on an ID it had read out of `session current`,
so a prompt nobody can see must not count as consent. Unconfirmable also
covers every cross-machine `--from`, where ancestry cannot speak to another
host.

**Adopt resolves against the COMBINED candidate set, and applies no ownership
rule of its own.** `strategy.IdentifyCallerSession` runs the identification
tier over this repository's states plus the source repository's, and adoption
is authorized exactly when the identified caller IS the session being adopted.

That boundary was arrived at by getting it wrong three times, each an
adopt-local rule layered on the shared one, and each diverging from it
somewhere:

1. The environment names the source session, so it is ours. No — an
   environment variable says only that *some* ancestor published it.
2. The source session's owner is somewhere in our ancestry, so it is ours.
   No — the outer agent of a nested pair really is an ancestor.
3. Consult the source store only when the resolver identifies nobody. No — a
   matching *inherited* claim makes the resolver identify the OUTER session,
   so the nearer inner owner in the source store is never examined.

Each fix corrected the divergence; combining the candidates removes it. Two
tests were deleted when this landed, and it is worth knowing why rather than
re-adding them: they asserted a "positive mismatch" and a "globally ambiguous"
verdict that only held *because* the first resolution omitted the source
states. Their premises were artifacts of the two-stage shape, not policy.

**A supplied state wins an ID collision.** `mergeSessionStates` prefers the
caller's copy over this repository's own, which is the opposite of what
concatenating the local listing first would give you. Colliding IDs are a
supported condition rather than a curiosity: `--force` exists precisely to
replace a state the target already holds for the session being adopted, so on
that path the local copy is by definition the stale one. Preferring it broke
both directions — a caller-owned source state shadowed by an ownerless
leftover refused a legitimate adoption, and a leftover still carrying an owner
from a previous adoption could authorize on evidence about a state nobody was
adopting. Merging the two copies' evidence instead (taking whichever has an
owner, say) would be worse: that is exactly how a stale owner record gets
combined with a live session.

**The shared policy is retained deliberately, including the depth-0
exemption.** A winner whose owner is our immediate parent cannot be beaten by
an unplaceable claim, so it is identified rather than reported ambiguous —
pinned from both sides by
`TestSessionAdopt_DepthZeroSourceOwnerWinsOverAnUnplaceableClaim` and
`TestSessionAdopt_UnplaceableClaimMakesADeeperSourceOwnerAmbiguous`, which
differ only in the winner's depth. Adoption therefore inherits the resolver's
acknowledged limit: a session equally near the winner and invisible to the
ranking. **If mutation ever needs a stricter standard than display does, that
belongs in the shared vocabulary — an explicit `IsSafeToMutate` alongside
`IsCaller` — not in an ordering private to `session_adopt.go`.** That is the
lesson of the three wrong shapes above.

**Why the source repository's states have to be candidates at all**, since
that is the part of the shape that is not obvious: `ResolveCallerSession`
resolves against the *current* repository's session store, so in a
cross-repository adoption the source state — and the owner process recorded on
it — is not in the listing it searches. For an agent that publishes a session
ID this is invisible, because the environment names the source session
directly and matches. For Gemini CLI and opencode, which publish nothing,
ancestry is the only signal and it was looking in the wrong place — so their
own cross-repo adoption, the case the command exists for, was refused, with
`--allow-foreign-session` as the only way through. Teaching an agent to waive
a real check in order to do a legitimate thing is worse than the check not
existing.

**Both listings must also be COMPLETE, and that is a separate question from
whether the resolver identified anybody.** Neither store listing is fatal on a
state file it cannot read — one corrupt file must not blind every command to
the rest of the store — so the loss arrives as a *successful* listing with a
candidate quietly absent. A missing candidate is exactly the nearer owner
whose absence lets a bare environment match win, so the guard refuses when
either listing was short, ahead of the verdicts rather than among them:
incompleteness undermines them rather than competing with them, and the one
branch it invalidates is the one that authorizes.

`session.StateStore.ListWithSkipped` reports what a listing could not read and
`ResolvedSession.Incomplete` carries it out of the resolver;
`SessionResolution.IsCaller()` deliberately does not fold it in, because the
resolution is still the best available answer and still worth *displaying* —
only mutation has to be conservative. Two properties are load-bearing:

- **Reporting the listing's error is not enough**, which is why this is a
  value rather than a log line. A store that cannot be opened at all is loud
  (every later read fails the same way, so the command stops on its own —
  measured: a mode-000 store directory already failed the adoption at the
  mutation step, before any of this), whereas a single unreadable *file*
  leaves the listing succeeding and no error channel says anything. Measured
  before the fix: one state file at mode 000 in the target store had a
  matching environment claim adopt silently, and had `entire session list`
  print "No sessions." over a store that held one.
- **Refusing is not walling the user out.** `refuseForeignAdoption` asks
  wherever there is a terminal, and `--allow-foreign-session` covers the rest,
  so a repo with one stale corrupt file is not stranded.

The display side says so rather than refusing, since nothing is being mutated:
`session list` and `session current` both warn on stderr — before the
not-found branch in `session current`'s case, because "no active session" over
a store that could not be read fully is the misreading, not the answer.


`--allow-foreign-session` is the escape hatch, and deliberately **not** folded
into `--force`/`--yes`: those already carry two meanings (replace local state,
confirm same-store adoption), and `--force` is the flag an agent reaches for on
any refusal. Waiving a safety check on someone else's running session gets a
name that says so.

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
