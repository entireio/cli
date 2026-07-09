# Test Plan: Git Remote Operations & Upstream Resolution — Both Checkpoint Backends

Status: proposal, 2026-07-04.
Scope: e2e (`e2e/tests/`) + integration (`cmd/entire/cli/integration_test/`) coverage for everything that talks to a git remote — pre-push checkpoint sync, remote/upstream resolution, cross-machine fetch — for the **git-branch** (`entire/checkpoints/v1`) and **git-refs** (`refs/entire/checkpoints/<shard>/<id>`) backends.

---

## 1. Where we stand

### What exists

| Layer | Coverage |
|---|---|
| Integration | `remote_operations_test.go` (12 tests: pre-push to bare file remote, checkpoint_remote routing, clone+resume, concurrent push rebase-retry, graceful degradation, filtered-fetch resume), `http_remote_test.go` (4 tests: in-process smart-HTTPS server, `ENTIRE_CHECKPOINT_TOKEN` injection, URL derivation, non-FF rebase, 401 degradation), `explain_test.go` (fetch-on-miss ×3 incl. treeless), `diverged_replay_test.go` |
| Strategy/checkpoint unit | `push_common_test.go`, `checkpoint_remote_test.go`, `refs_push_test.go` (FF-only, fetch+replay recovery), `manual_commit_opf_rewrite_test.go`, `safely_advance_local_ref_test.go`, `replay_disconnected_test.go`, `pushqueue_test.go`, `checkpoint/remote/util_test.go` |
| E2E | `SetupBareRemote` helper (opt-in; default harness repo has **no remote**); 4 remote-flavored tests: `resume_remote_test.go` (clone+auto-fetch ×2), `explain_test.go` (explain from clone), `doctor_test.go`, `alternates_test.go` (the only pre-push-hook e2e — **skipped under git-refs**) |
| Backend matrix | e2e only: `E2E_CHECKPOINT_STORE` → `ENTIRE_CHECKPOINTS_PRIMARY` (`e2e/testutil/backend.go`); CI canary runs both. **Integration tests have zero git-refs parameterization.** |

### Structural blind spots

1. **The real git-invoked pre-push hook is never asserted end-to-end.** Integration `RunPrePush` spawns `entire hooks git pre-push <remote>` with `Stdin = nil` (`testenv.go:1950`) — real git feeds `<local-ref> <sha> <remote-ref> <sha>` lines on stdin. `GitPush` always passes `--no-verify` (`testenv.go:1926`). E2E pushes *do* run the installed hook, but every remote e2e test then calls `PushCheckpointRefs` explicitly, so a completely broken hook would go unnoticed.
2. **The read/fetch side hardcodes `origin` everywhere** (`checkpoint/remote/util.go:18`, `git_operations.go:363-443`, `strategy/common.go`, `resume.go`, `api_cmd.go:216`, …) while the push side follows the hook's `$1`. No test has a repo whose only remote is `upstream`, a multi-remote fork triangle, or a push to a non-origin remote followed by reads.
3. **Upstream tracking config is never consulted** (`branch.<name>.remote`, `remote.pushDefault`, separate `pushurl`) — and never pinned by a test, so we don't even document the current behavior.
4. **git-refs remote behavior is unit-tested only**: batch FF-only push, fetch+replay recovery, queue drain/compaction/stale-pruning, on-demand `RefFetcher` — nothing at integration level, and the one hook-driven e2e skips under git-refs.
5. **OPF pre-push rewrite has zero integration/e2e coverage** (divergence abort, CAS `V1RefMovedError`, bootstrap cap — all unit-only).
6. **No worktree+remote coverage at all** (the git-refs push queue lives in the git *common* dir, shared across worktrees — never exercised with >1 worktree).
7. **No SSH remotes anywhere; no shallow user clones; detached HEAD only as setup plumbing.**

### History says these regress (mined from PRs/commits)

- **Destructive local-v1-ref moves** — the single most re-broken area: #953, `96034892c`, #1252, #1251, #1260 (ahead/behind/diverged/disconnected/shallow × resume/explain/doctor/pre-push).
- **Cross-clone replay fidelity**: no-op commit tree clobber (`743c43f4c` — *no test*), >1000-commit replay cap (`4cf01edb3` — *no test*), stale ls-remote hash TOCTOU (`1e8628ade` — *no test*), double-replay (#1260 has a test).
- **Remote-target precedence**: hardcoded-origin blob fetch (#976), `entire://`/`file://` non-derivable origin (#1279), token SSH→HTTPS coercion on fetch missing (`7afdaa33e`), silent origin fallback faking success (`53bc37a88`).
- **Errors masked as not-found**: partial-clone `ErrFileNotFound` (#1069), git-refs read paths (`7bbdad09c`).
- **Self-inflicted shallow grafts** (#1443), promisor config pollution of `[remote "origin"]` (#934), gc pack race (#1276).
- **Hook/transport hangs**: grandchild helper holding pipes (#1282), timeout stacking (`2e2c1b73a`), protected-ref GH013 silence (#1033).
- **Hermeticity**: tests accidentally hitting live github.com / macOS keychain (#1463, `53bc37a88`).

---

## 2. Infrastructure work (enablers, do first)

**I-1. Integration backend matrix.** Add a `CheckpointStore` option to `TestEnv` (sets `ENTIRE_CHECKPOINTS_PRIMARY`, inherited by spawned binaries/hooks like the e2e TestMain does) plus a `ForEachBackend(t, func(t, env))` helper. Run the whole remote-touching integration surface (`remote_operations_test.go`, `http_remote_test.go`, explain fetch-on-miss, diverged replay) under both backends; use targeted skips where behavior legitimately differs (e.g. OPF is git-branch-only).

**I-2. Real-hook push in tests.**
- Integration: `GitPushWithHooks` (no `--no-verify`) so the installed pre-push script runs exactly as git runs it (stdin refspec lines, remote name + URL argv). Keep `GitPush` for setup plumbing.
- Fix `RunPrePush` to feed realistic stdin lines instead of nil (or replace its uses with `GitPushWithHooks`).
- E2E: assertion helper `AssertCheckpointsOnRemote(t, s, bareDir)` that checks the backend-appropriate refs (`entire/checkpoints/v1` vs `refs/entire/checkpoints/*`) landed on the remote **without** calling `PushCheckpointRefs`.

**I-3. Remote-topology helpers.** On top of the existing `SetupBareRemote`/`SetupNamedBareRemote`/`CloneFrom`: `SetupUpstreamOnlyRemote` (only remote named `upstream`), `SetupForkTriangle` (origin = fork, upstream = base), `AddWorktree(t, env, branch)` returning a second working dir sharing the common git dir.
- Reuse `startGitHTTPSServer` (`http_remote_test.go:54`) wherever URL parsing matters (fork detection, token flows) — file-path remotes can't exercise it (`remote_operations_test.go:178-187`).

**I-4. Hermeticity guard.** A TestMain-level tripwire for the integration suite: fail any test whose git commands dial a non-loopback host (e.g. `GIT_CONFIG_COUNT`-injected `url.<base>.insteadOf` redirect to an invalid local address, or asserting `ENTIRE_CHECKPOINT_TOKEN` is unset unless the test sets it). Regression class: #1463, `53bc37a88`.

---

## 3. Test cases

Legend: **[gb]** git-branch, **[gr]** git-refs, **[both]** run under the I-1 matrix. Priority P0 = past data-loss/regression or zero coverage on a core flow; P1 = important hardening; P2 = nice-to-have/pinning.

### A. Real pre-push hook, end to end — P0

| # | Test | Layer | Backend |
|---|---|---|---|
| A1 | Plain `git push` of a feature branch (real hook, stdin refspecs) lands checkpoints on the bare remote; nothing else about the user push changes | integration + e2e | both |
| A2 | `git push` of a branch with **no** new checkpoints → hook is a fast no-op, user push succeeds | integration | both |
| A3 | `git push --delete <branch>` and tag-only pushes through the hook (stdin shape differs — zero-sha lines) — no checkpoint push attempted, no crash | integration | both |
| A4 | Hook failure containment: unreachable checkpoint remote → user push still succeeds with warning (today only tested via nil-stdin `RunPrePush`) | integration | both |
| A5 | e2e: full agent session → user commits → plain `git push` → clone in fresh dir → `entire resume` works **without any explicit `PushCheckpointRefs`** (closes the masking gap) | e2e (vogon-first) | both |
| A6 | git-refs: `TestAlternates`-equivalent pre-push coverage — hook-driven push of queued per-checkpoint refs to a seeded bare remote, asserting FF and queue emptied (replaces the git-refs skip) | e2e | gr |

### B. Remote-name & upstream resolution — P0

These mostly **pin current behavior first**; several will surface product decisions (see §4).

| # | Test | Layer | Backend |
|---|---|---|---|
| B1 | Repo whose only remote is `upstream`: enable, checkpoint, `git push upstream` → checkpoints go to `upstream` (hook `$1`); then `resume`/`explain`-from-clone document the current origin-hardcoded read failure (pin, referencing decision D-1) | integration | both |
| B2 | Fork triangle (origin = fork, upstream = base): push to origin syncs checkpoints to origin; push to upstream — where do checkpoints go, and do subsequent reads find them? (pin) | integration | both |
| B3 | No remote at all: pre-push not applicable, but doctor/reconcile OK ("no remote to compare", regression `cac63b010`), `entire api` `{owner}` substitution errors cleanly, status/enable don't crash | integration | both |
| B4 | `branch.<name>.remote=upstream` with both remotes present: pin that checkpoint reads still target origin; `git push` (no args, tracking-based) routes hook `$1`=upstream — assert consistent checkpoint destination | integration | both |
| B5 | checkpoint_remote fork detection over HTTPS server: push-remote owner ≠ checkpoint_remote owner → fallback used (today unit-only; run through a real push) | integration (HTTPS) | gb (gr: N/A until checkpoint_remote applies to refs — verify and pin) |
| B6 | `ENTIRE_CHECKPOINT_TOKEN` with SSH-shaped origin URL (`git@host:o/r.git` pointing at the HTTPS test server via insteadOf or URL rewrite): fetch **and** push both coerce to HTTPS (regression `7afdaa33e`) | integration (HTTPS) | both |
| B7 | Local-path origin (bare dir added as `origin`): `ParseURL` fails → raw-origin fallback still pushes/fetches correctly; no crash in URL derivation (pin; latent gap `isLocalPath`) | integration | both |

### C. Cross-machine: clone → fetch → resume/explain/attribution — P0

| # | Test | Layer | Backend |
|---|---|---|---|
| C1 | Production `fetchMetadataBranchIfMissing` path exercised for real: clone without checkpoint refs, run a pre-push settings resolution (e.g. first `git push` from the clone) → v1 fetched. Today integration substitutes raw `git fetch` (`remote_operations_test.go:316`) | integration | gb |
| C2 | git-refs on-demand `RefFetcher`: clone, `entire explain <id>` / `resume` / `tokens` fetch exactly the needed ref (assert no v1 branch fetch, no other refs) | integration | gr |
| C3 | git-refs offline read: unreachable remote + locally-missing ref → real error, **not** "checkpoint not found" (regression `7bbdad09c`) | integration | gr |
| C4 | Clone over HTTPS with token: resume auto-fetch works with auth (e2e today is file-path only) | integration (HTTPS) | both |
| C5 | Shallow user clone (`git clone --depth=1`): enable, session, commit, push, resume — no `.git/shallow` self-infliction (regressions #1443, #1276), replay refuses at shallow boundary rather than corrupting | integration | both |
| C6 | Partial clone (`--filter=blob:none`): explain/attribution lazily fetch blobs via `fetch-pack`, no promisor config stamped onto `[remote "origin"]` (regressions #1069, #934 — extend the existing config-guard) | integration | both |

### D. Divergence & recovery matrix — P0 (the most re-broken area)

Systematize the ahead/behind/diverged/disconnected × operation matrix that items #953/#1251/#1252/#1260 patched piecemeal:

| # | Test | Layer | Backend |
|---|---|---|---|
| D1 | Table-driven: local v1 {ahead, behind, diverged, disconnected(no merge-base), missing} × trigger {resume, explain fetch-on-miss, pre-push, doctor} → local-only commits always survive; behind → FF; diverged → replay preserving both sides; count assertions guard double-replay | integration | gb |
| D2 | Same matrix for git-refs per-checkpoint refs: remote-ahead ref (another clone amended) → fetch+replay+non-force retry lands both; genuine conflict → ref stays queued, remote untouched, next push retries | integration | gr |
| D3 | Concurrent pushers from two clones (extend `TestConcurrentPush_...` to git-refs; the v1 version exists) | integration | gr |
| D4 | Replay fidelity edges: no-op commit replay must not clobber the remote tip's tree (`743c43f4c` — currently untested); root-commit replay; >1000 local-only commits push successfully (`4cf01edb3` — currently untested) | integration or strategy unit | gb |
| D5 | TOCTOU: remote advances between ls-remote and fetch → reconcile uses fetched hash (`1e8628ade` — currently untested; simulatable with a pre-fetch hook on the bare remote or an interposed fetch) | strategy unit/integration | gb |
| D6 | Multi-worktree git-refs queue: two worktrees enqueue checkpoints; `git push` from worktree A drains the shared common-dir queue — worktree B's refs also pushed, queue compacted; stale refs pruned | integration | gr |
| D7 | Pin the known two-remote queue bug: push to remote A drains the queue, so remote B never receives those refs (decision D-2 — pin now, flip assertion when fixed) | integration | gr |

### E. OPF pre-push rewrite (git-branch only) — P1

| # | Test | Layer |
|---|---|---|
| E1 | OPF enabled, happy path over a real bare remote via real hook: unpushed v1 commits rewritten with `Entire-OPF-Applied: true`, pushed; remote content is the redacted version | integration |
| E2 | Diverged v1 during rewrite → `V1DivergedError` aborts the **user** push with the actionable message | integration |
| E3 | CAS conflict: concurrent checkpoint write between rewrite and ref-update → `V1RefMovedError`, re-run push succeeds | integration |
| E4 | Bootstrap cap: remote has no v1 + local history over cap → typed abort, nothing pushed | integration |
| E5 | Non-TTY push with OPF (regression `626a0344e`): no /dev/tty writes, hook completes | integration |

### F. Degraded remotes & protocol edges — P1

| # | Test | Layer | Backend |
|---|---|---|---|
| F1 | Protected v1 branch (GH013 emulation via bare-remote `pre-receive` hook): loud banner, no retry loop, user push unaffected (regression #1033 — currently unit-only on output classification) | integration | gb |
| F2 | Hook time bounds: unreachable/hanging remote (HTTPS server that accepts then stalls) → pre-push respects the shared push budget, no per-attempt timeout stacking (regressions #1282, `2e2c1b73a`) | integration (HTTPS) | both |
| F3 | 401→token-retry over HTTPS for git-refs batch push (v1 version exists: `TestHTTPS_PushFailsWithoutToken`) | integration (HTTPS) | gr |
| F4 | Checkpoint policy sync in the git-refs pre-push path (regression `7bbdad09c` — policy check was skipped): blocked policy skips checkpoint refs but not the user push | integration | gr |
| F5 | Detached HEAD: session + checkpoint while detached; `git push origin HEAD:branch`; resume from detached clone (today detach is only setup plumbing) | integration | both |
| F6 | `entire://` origin with checkpoint_remote configured → provider-host routing, not a push at the helper (regression #1279) — currently unit-only; needs a fake provider mapping or injectable host table | integration | gb |

### G. E2E additions (real agents optional, vogon default) — P1

- G1: extend `resume_remote_test.go` + `explain_test.go` clone tests to rely on the real hook (drop `PushCheckpointRefs`, see A5) and run under both `E2E_CHECKPOINT_STORE` values in the CI canary matrix.
- G2: one e2e worktree scenario: session in a linked worktree, commit, push from the worktree, clone elsewhere, resume (covers worktree shadow-branch namespace + shared queue end-to-end).
- G3: doctor e2e on a repo with unreachable remote (today `TestDoctorNoIssues` only covers healthy).

### H. Explicit non-goals (for now)

- Real SSH transport (sshd in CI) — the SSH-specific logic is URL parsing/coercion, covered at unit + B6; a real sshd adds flake for little marginal signal.
- GitLab token semantics, GHE forge mapping — product questions before tests (see D-3 below).
- `git-remote-entire` replica failover internals — separately tested in `internal/remotehelper`.

---

## 4. Product decisions the tests will force (flag before pinning)

- **D-1 non-origin reads**: pushing checkpoints to `upstream` (hook `$1`) while every read path fetches from `origin` is incoherent. Decide: teach reads to use the checkpoint-bearing remote (e.g. remember last push remote, or consult `branch.<name>.remote`), or document origin-only support and warn on non-origin pushes. B1/B2/B4 pin whichever is chosen.
- **D-2 multi-remote queue clearing** (git-refs): queue entries are deleted after a successful push to *any* remote — second remote permanently misses refs. Probably needs per-remote tracking or "delete only when pushed to the fetch-resolution target". D7 pins current behavior until then.
- **D-3 forge map / provider table**: only `github.com`→`gh` and github/gitlab provider hosts exist; GHE/self-hosted silently degrade (`{repo_id}`, trails). Decide config story before writing tests beyond pinning.

## 5. Suggested sequencing

1. **PR 1 — infrastructure**: I-1 (backend matrix for integration), I-2 (real-hook push helpers + RunPrePush stdin), I-4 (hermeticity tripwire). Immediately re-run the existing remote suites under git-refs; expect it to surface real bugs the same way the e2e matrix did (`refs-v1` policy, explain-clone fetch).
2. **PR 2 — P0 hook & cross-machine**: A1–A6, C1–C4, plus e2e G1.
3. **PR 3 — divergence matrix**: D1–D7 (D4/D5 fill known untested regressions).
4. **PR 4 — remote-name/upstream pinning**: B1–B7 after a decision on D-1 (or with explicit "pins current behavior" markers).
5. **PR 5 — OPF + degraded**: E1–E5, F1–F6, G2–G3, C5–C6.

Rough sizing: PRs 1–3 are the high-value core (~2/3 of the risk reduction, all P0); 4–5 can trail.
