# Session Binding

Session binding makes one agent session show up in every repository it touched,
not only the one it was launched from. An agent started in `~/dev/acme` (no
repo) or in repo A can edit files in repo B; without binding, B never sees a
session, never gets a checkpoint, and its commits never link. Binding is
evidence-driven, additive, and best-effort: it never blocks the agent, never
modifies the launching repo's session, and only ever acts on repos Entire is
active in (`settings.IsActiveAtRoot` — the repo's own settings enable it OR
[global tracking](global-tracking.md) covers it).

## Pieces

| Piece | Where | What it does |
| --- | --- | --- |
| Evidence tap (in-repo) | `binding_tap.go` | At turn end, transcript-extracted paths that land outside the current worktree (the `FilterAndNormalizePaths` clamp's rejects), plus kept paths inside an unregistered repo nested under the worktree, are resolved to their repos |
| Evidence path (no repo) | `binding_norepo.go` | When the hook's cwd is not a repo: tool-use payload paths (joined on the tool's own cwd) and a cursor-driven scan of the transcript's new lines |
| Machine record | `binding/record.go` — `userdirs.Config()/sessions/<session>.json` | One record per session: agent, transcript, launch root, every bound repo with its worktree root, canonical git common dir, `enabled`, evidence count, and `adopted_at`; plus the transcript scan cursor |
| Repo resolution | `binding/resolve.go` | `git rev-parse --show-toplevel --git-common-dir` per distinct directory, cached per process; canonical (symlink-resolved) paths so one clone has one `CommonDir` key |
| Adoption | `binding_adopt.go` | Replicates the session state into an active target's common dir |
| Replay | `binding_checkpoint.go` | Re-runs the turn-end hook inside each replicated target so it takes its own checkpoint |

Per-turn bounds keep the hook path cheap: at most 16 distinct directories are
resolved (one `git` fork each) and at most 8 foreign repos recorded per turn;
the no-nested-repo common case costs zero forks.

## Adoption

For each bound repo that is active, `ensureSessionReplicated` writes an
additive replica of the session state into the target's
`<common dir>/entire-sessions/`: same session ID, agent, transcript path and
start time; target-local base commit, worktree path/ID, branch, untracked-file
baseline; checkpoint bookkeeping reset. The source state is never retired or
rewritten — the same session is simply active in two repos.

Rules that are load-bearing:

- **Activity is re-checked immediately before the write** so a concurrent
  disable or exclusion stays an absolute veto.
- **The existence check runs under the target's session-state lock**, for a
  marked and an unmarked repo alike. The record's `adopted_at` is a hint;
  a replica removed by cleanup is rebuilt from the next evidence.
- **The lock timeout (2s) bounds only the acquire.** The work inside runs
  under the hook's own context (agent deadline and Ctrl-C still apply), and
  the target's status walk uses `gitrepo.StatusWithIsolatedBudget` with a 5s
  budget: a slow foreign repo neither holds the target's lock for long nor
  arms the process-wide status latch that would degrade the launching repo's
  own capture. That walk seeds the replica's baselines — untracked files,
  already-dirty tracked paths and already-deleted tracked paths
  (`DirtyTrackedFilesAtStart`, `DeletedTrackedFilesAtStart`). If the walk
  fails, adoption is deferred to the next turn rather than persisting a
  replica that would credit every pre-existing change to the session.
- **A linked worktree of the launching clone is never a target.** Session
  state is shared across a clone's worktrees, so the "target store" would be
  the source's own; its evidence is recorded, nothing is replicated, and
  commit linking's identity-first matching covers multi-worktree sessions.
- **A failed `adopted_at` write does not withhold the replay**: the replica
  exists, which is what "replicated" means; the failure is logged.
- A target with an unborn HEAD cannot be adopted (no base commit).

## Replay

After the launching process finishes its own turn-end work, it re-runs
`entire hooks <agent> <verb>` with the original payload, in parallel, inside
every repo replicated this turn, with `ENTIRE_BINDING_REPLAY=1` and
`ENTIRE_BINDING_PRIMARY=1|0`. Each child is bounded (30s, `WaitDelay` 2s); its
stderr is captured into the error and a failure logs at Warn — it is a
checkpoint the target silently missed.

Inside a replay child:

- A replay child never has a pre-prompt baseline (only TurnEnd is replayed),
  so tracked changes are credited to the turn only when they are not in the
  replica's tracked baselines — the user's pending edits stay out; the
  agent's edits and deletions, which no transcript names, stay in, including
  a deletion of a file that was merely dirty before — and new files only when
  the transcript evidences them. At the end of every replayed turn the
  baselines are rewritten to the post-turn tree, so whatever the user changes
  in that repo between turns is measured against the tree the last turn left.
- **Tokens exist once per turn.** The token-primary repo (latest evidence)
  puts them on its checkpoint; every other repo still folds them into its
  session-wide total (`StepContext.TokensAttributedElsewhere` — the transcript
  is shared, so `entire status` reports the same total everywhere) but not
  into its pending checkpoint delta.
- The tap does not run (`ENTIRE_BINDING_REPLAY` short-circuits it), so a
  replay never fans out again.

Known limits: the launching process decides token attribution before the
replay runs, so if the primary repo's replay child fails or times out, that
turn's tokens land on no checkpoint (the session totals are unaffected). And
since the adopted marker is only a hint, every turn re-takes the target's
lock for each already-adopted repo — correct, but no longer free.

Commit linking is unchanged: a commit in the target links to the session
through the identity-first matcher in `strategy/session_identity.go`.

## Failure policy and logging

Everything here is best-effort: a panic in the tap is swallowed, every error
is logged and the turn continues, and the agent is never blocked. In a repo,
binding failures log at Warn (the repo has a log). The no-repo path has no
file sink; it attaches `logging.Discard()` so nothing falls through to slog's
default stderr handler — the agent's stderr.
