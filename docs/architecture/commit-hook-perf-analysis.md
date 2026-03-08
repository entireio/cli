# Commit Hook Performance Analysis

## Test Results (2026-02-27)

Measured on a full-history single-branch clone of `entireio/cli` with 200 seeded branches and packed refs.
Each session generated with a unique base commit from repo history. ENDED sessions are split 75/25
between shadow-branch sessions (expensive path) and committed sessions (cheap path), matching
production distribution observed in `.git/entire-sessions/`.

| Scenario | Sessions | Control | Prepare | PostCommit | Total | Overhead |
|----------|----------|---------|---------|------------|-------|----------|
| 100      | 100      | 29ms    | 815ms   | 6.491s     | 7.306s | 7.276s  |
| 200      | 200      | 20ms    | 1.651s  | 14.629s    | 16.28s | 16.26s  |
| 500      | 500      | 29ms    | 4.433s  | 46.934s    | 51.37s | 51.34s  |

**Scaling: ~73ms per session at 100, ~81ms at 200, ~103ms at 500.** PostCommit dominates overwhelmingly.
Control commit (no Entire) is ~25-30ms regardless of session count.

The 200-session result (**16.28s**) closely matches the real-world user report of **~16s for ~95 sessions**,
confirming the test methodology now faithfully reproduces production overhead.

### Session distribution per scenario

| Scenario | ENDED (shadow) | ENDED (committed) | IDLE | ACTIVE |
|----------|---------------|-------------------|------|--------|
| 100      | 66            | 22                | 11   | 1      |
| 200      | 132           | 44                | 22   | 2      |
| 500      | 330           | 110               | 55   | 5      |

### Impact of test methodology

This test went through several iterations to achieve realistic numbers:

| Version | 100 sess | 200 sess | 500 sess | Per-session | Issue |
|---------|----------|----------|----------|-------------|-------|
| Shallow + shared base | 1.74s | 3.59s | 9.52s | ~18ms | Packfile too small, repeated ref scan |
| Full history + shared base | 2.00s | 4.16s | 10.9s | ~21ms | Same ref scanned N times |
| Full history + unique bases (cheap ENDED) | 337ms | 617ms | 1.52s | ~3ms | ENDED sessions had LastCheckpointID → no-ops |
| **Full history + realistic ENDED (current)** | **7.3s** | **16.3s** | **51.4s** | **~73-103ms** | **Matches production** |

The critical fix was making ENDED sessions realistic: 75% have shadow branches with data but **no** `LastCheckpointID`.
These exercise the full expensive path: ref resolution → commit/tree resolution → transcript/overlap checking →
condensation during PostCommit. Previously, all ENDED sessions had `LastCheckpointID` set, making them trivial no-ops
that skipped the entire hot path.

## How go-git `repo.Reference()` works

go-git has **no caching** for packed ref lookups. Each `repo.Reference()` call:
1. Tries to read a loose ref file (`.git/refs/heads/<name>`)
2. On miss, opens `packed-refs` and scans line-by-line until match or EOF
3. For refs that don't exist, scans the **entire** file every time

After `git pack-refs --all` (the default state after `git gc`), all refs are in packed-refs and loose ref files don't exist. This means every lookup scans the file.

## Scaling Dimensions

### 1. PostCommit condensation — the dominant cost (~50-80ms/session)

When PostCommit processes an ENDED session with new content (shadow branch exists, no `LastCheckpointID`),
it triggers the full condensation pipeline:

1. **Ref resolution**: `repo.Reference()` to find shadow branch (~1ms)
2. **Commit/tree resolution**: Resolve commit object and tree from shadow branch ref (~1ms)
3. **Content detection**: `sessionHasNewContent()` checks transcript or FilesTouched overlap (~2-5ms)
4. **State machine transition**: ENDED + GitCommit → ENDED with `ActionCondense` (~0.5ms)
5. **Condensation**: Read shadow branch data, write to `entire/checkpoints/v1` branch (~30-50ms)
6. **Shadow branch cleanup**: Delete alias ref after successful condensation (~1-2ms)
7. **Session state update**: Set `LastCheckpointID`, clear `FilesTouched` (~0.5ms)

The condensation step dominates because it creates commits on the metadata branch with full tree building.

### 2. `repo.Reference()` — ref lookups (~2-4ms/session)

Every session triggers multiple git ref lookups via go-git's `repo.Reference()`:

| Call site | When | Per-session calls |
|-----------|------|-------------------|
| `listAllSessionStates()` (line 91) | Both hooks | 1× |
| `filterSessionsWithNewContent()` → `sessionHasNewContent()` (line 1131) | PrepareCommitMsg | 1× |
| `postCommitProcessSession()` (line 840) | PostCommit | 1× |

PostCommit pre-resolves the shadow ref at line 840 and passes `cachedShadowTree` to avoid redundant lookups within that hook.

### 3. `store.List()` — session state file I/O (~0.5-1ms/session)

`StateStore.List()` does `os.ReadDir()` + `Load()` for every `.json` file in `.git/entire-sessions/`. Each `Load()` reads a file, parses JSON, runs `NormalizeAfterLoad()`, and checks staleness. Called once per hook via `listAllSessionStates()` → `findSessionsForWorktree()`.

### 4. Content overlap checks (~2-5ms/session, conditional)

`stagedFilesOverlapWithContent()` (PrepareCommitMsg) and `filesOverlapWithContent()` (PostCommit) compare staged/committed files against `FilesTouched`. Triggered for sessions with shadow branches and `FilesTouched` set.

## Cost Breakdown Per Session (ENDED with shadow branch)

| Operation | Cost | Notes |
|-----------|------|-------|
| `repo.Reference()` | 2-4ms | 2-3 lookups across both hooks |
| `store.Load()` (JSON parse) | 0.5-1ms | Per session state file |
| Content detection | 2-5ms | Transcript or overlap check |
| **Condensation** | **30-50ms** | **Dominant cost** — tree building + commit creation |
| Shadow branch cleanup | 1-2ms | Delete ref after condensation |
| **Total per session** | **~40-60ms** | |

## Why PostCommit dominates

PrepareCommitMsg is relatively fast (~8ms/session) because it only does content detection
(ref lookup + tree inspection + overlap check). It does NOT trigger condensation.

PostCommit adds the full condensation cost on top of content detection. For each ENDED session
with new content:
- Reads transcript/metadata from shadow branch tree
- Builds a new tree on `entire/checkpoints/v1`
- Creates a commit with checkpoint metadata
- Updates session state with `LastCheckpointID`
- Deletes the shadow branch ref

This creates a **multiplicative** cost: N sessions × condensation cost per session.

## Optimization Opportunities

### Critical impact (address PostCommit condensation)

1. **Batch condensation**: Instead of condensing sessions one-by-one (each creating a separate commit on `entire/checkpoints/v1`), batch all sessions into a single commit. This would reduce N commits to 1 commit.

2. **Prune stale ENDED sessions aggressively**: Sessions older than `StaleSessionThreshold` (7 days) that have shadow branches but no `LastCheckpointID` create unnecessary condensation work. Proactive cleanup would reduce the session count.

3. **Session pruning during PostCommit**: Before condensing, skip ENDED sessions that are clearly stale (e.g., > 7 days without interaction, no overlap with committed files).

### High impact (reduce ref scanning)

4. **Skip orphan check for ENDED sessions with `LastCheckpointID`**: These sessions survive the check at line 92 anyway. Short-circuiting before `repo.Reference()` would eliminate ~25% of ref lookups in `listAllSessionStates()`.

5. **Batch ref resolution**: Load all refs once into a map for O(1) lookups instead of scanning packed-refs per session.

6. **Cache shadow ref across hooks**: The ref resolved in `listAllSessionStates()` is thrown away and re-resolved in `filterSessionsWithNewContent()`. Threading it through would avoid redundant lookups.

### Medium impact

7. **Lazy condensation**: Instead of condensing during PostCommit (synchronous, blocking the commit), defer condensation to a background process or the next session start.

8. **Use `CheckpointTranscriptStart` instead of re-parsing transcripts**: Avoid full JSONL parsing by comparing against a stored line count.

## Reproducing

```bash
go test -v -run TestCommitHookPerformance -tags hookperf -timeout 15m ./cmd/entire/cli/strategy/
```

Requires GitHub access for cloning. Sessions are generated from repo commit history (no external templates needed).
