# Validating `migrate-v2-checkpoints`

Reusable runbook for verifying that `migrate-v2-checkpoints` (read-only or applied)
identifies the correct checkpoints, attributes the correct sessions, and — once
applied — writes complete, hash-consistent data to the v1 branch.

Tested against the `tmp-migrate-v2-script-go` branch of the CLI at
`~/entire/cli/.worktrees/review`. The binary lives there as
`migrate-v2-checkpoints`.

> Background: the project is rolling **back** checkpoints v2. v2 stores live
> under `refs/entire/checkpoints/v2/*` and are no longer being written. The
> v1 branch `entire/checkpoints/v1` is the surviving format. This tool reads
> v2 metadata + raw transcripts and replays them as v1 writes via
> `checkpoint.GitStore.WriteCommitted`.

> ⛔ **DO NOT push `entire/checkpoints/v1` to any remote at any point while
> following this runbook.** The migration writes new commits to the local
> v1 branch and nothing else. Publishing those commits is a separate,
> manual decision the operator makes **only after** §5 validation has
> fully passed and they are satisfied with the result. Pushing early
> propagates any bad migration to every consumer (other clones,
> `checkpoint_remote`, the API) and makes rollback significantly more
> expensive than a local `update-ref`. If you are not sure whether you
> are about to push, you are not ready to push.

## 1. What the tool does

### 1.1 Discovery (`cmd/migrate-v2-checkpoints/history.go`)

- Walks every history tip (branches under `refs/heads/*` and `refs/remotes/*/*`,
  excluding `entire/checkpoints/v1`, `entire/trails/v1`, and any `*/HEAD`
  symbolic ref). Falls back to `HEAD` if no other tips qualify.
- For each commit on those tips, parses `Entire-Checkpoint: <id>` trailers
  (`trailers.ParseAllCheckpoints`, key constant
  `trailers/trailers.go:41`). One commit can carry many trailers (squash
  merges).
- After the trailer walk, lists every checkpoint ID on
  `refs/entire/checkpoints/v2/main` (`addV2OrphanCheckpoints`). Any v2 /main
  ID not already discovered through a commit trailer is appended as an
  **orphan** — a `discoveredCheckpoint{ID, Commits: nil}` with no commit
  attribution. Orphans flow through the migration filter the same way as
  commit-attributed candidates; only their reporting label differs.
- Produces a list of `discoveredCheckpoint{ID, Commits}` — every checkpoint
  ID ever referenced in commit history plus every v2 /main ID, sorted by ID.
- `--since <commit>`/positional commit narrows to commits not reachable from
  the named commit. `--head <commit>` restricts to a single tip. **Either
  flag suppresses the v2 /main orphan augmentation**: when commit scope is
  set the tool re-runs the trailer walk unscoped, counts how many v2 /main
  IDs would have been newly discovered as orphans, and prints
  `warning: N v2 orphans skipped; re-run without --since/--head to include
  them` to stdout before the report. Those IDs are **not** added to the
  migration plan in the scoped run.
- Discovery is **not** v2-specific by default, but the orphan augmentation
  reaches into v2 /main, so v2 refs (or at least the local copy) influence
  the candidate set.

### 1.2 Migration filter (`cmd/migrate-v2-checkpoints/migration.go`)

For each discovered checkpoint:

1. Read v1 summary from `entire/checkpoints/v1`. If present, collect existing
   v1 session IDs by reading each session's `metadata.json` (`session_id`
   field). v1 session paths are recovered from
   `summary.Sessions[*].Metadata` via `v1SessionIndexFromSummary`, so sparse
   or non-contiguous v1 indices are handled correctly.
2. Read v2 summary from `refs/entire/checkpoints/v2/main`. If absent or has
   no sessions → `missing v2 checkpoint metadata` and skip.
3. For every session index in the v2 summary:
   - Read v2 session metadata + prompts from `/main`. Missing or empty
     `checkpoint_id` / `session_id` → `missing required v2 session metadata`.
   - If that session ID already exists in v1 → `already present v1 sessions`.
   - Read v2 raw transcript from `/full/current`, falling back to archived
     `/full/<13-digit-suffix>` refs. `ErrNoTranscript` →
     `missing raw transcripts`.
   - Otherwise: count `sessions eligible for migration`, and on `--apply`
     resolve the v2 `/main` commit that last touched that session's
     `metadata.json`, then write to v1 via `GitStore.WriteCommitted` using
     v2-sourced fields and that original v2 commit author line. The transcript
     is wrapped in `redact.AlreadyRedacted(...)` so the v1 writer does not
     re-redact bytes that were already redacted on v2.

A checkpoint is **eligible** if at least one v2 session is missing from v1 and
fully readable from v2. The candidate's `sessions=N` is that net count, not
the v2 session count.

Additionally, the report tracks how many eligible checkpoints were orphans
(discovered through v2 /main alone, with no commit trailer attribution). An
eligible checkpoint with `len(discovered.Commits) == 0` increments the
`v2 orphan checkpoints eligible for migration` counter; this is a subset of
`checkpoints eligible for migration`, never larger than `EC`.

### 1.3 What ends up on v1 after `--apply`

For each migrated session, the v1 tree at `<id[:2]>/<id[2:]>/<N>/` gains:

| file               | source                       | constant in `paths/paths.go`        |
|--------------------|------------------------------|-------------------------------------|
| `metadata.json`    | v2 session `metadata.json`   | `MetadataFileName` (line 36)        |
| `prompt.txt`       | v2 session prompts (joined)  | `PromptFileName` (line 29)          |
| `full.jsonl[.NNN]` | reassembled v2 `raw_transcript[.NNN]` | `TranscriptFileName` (line 30) |
| `content_hash.txt` | `sha256:<hex>` of v1 bytes  | `ContentHashFileName` (line 38)     |

Plus the root `<id[:2]>/<id[2:]>/metadata.json` gets rewritten to add the new
session to `sessions[]` and recompute aggregate fields (see §3.2).
Each migrated v1 metadata-branch commit uses the author name, email, and author
timestamp from the v2 `/main` commit that wrote the corresponding v2 session
`metadata.json`; the session metadata's own `created_at` remains the v2 JSON
value.

`<N>` is the v1 slot. New sessions append (`findSessionIndex` in
`committed.go:610`); if v1 already had session 0 and v2 contributes one new
session, it lands in v1 slot 1. v1 indices and v2 indices for the **same**
checkpoint can differ; `session_id` is the stable cross-store identifier.

Chunking note: `full.jsonl` is chunked via `agent.ChunkTranscript`. Chunks
are `full.jsonl`, `full.jsonl.001`, `full.jsonl.002`, …
(`agent/chunking.go:126` `ChunkFileName`, with
`ChunkSuffix = ".%03d"` at line 19). Index 0 has no suffix.

Codex caveat: for sessions whose agent is `codex`, `writeTranscript` applies
`codex.SanitizePortableTranscript` before chunking and hashing
(`committed.go:746`). The bytes written to v1 may differ from the bytes
read out of v2's `/full/*`, but they are still self-consistent against the
new v1 `content_hash.txt`.

## 2. Run modes & expected report shape

```text
$ migrate-v2-checkpoints [--repo PATH] [--since SHA | SHA] [--head SHA] \
                         (--list | --dry-run | --apply)
```

Default mode is `plan` (same output as `--dry-run`).

For `plan`, `--dry-run`, and `--apply` (but not `--list`), the tool resolves
the checkpoint fetch remote and refreshes the local v1 branch plus v2 refs
before discovery:

- `refs/heads/entire/checkpoints/v1` via `ensureLatestV1Ref`
- `refs/entire/checkpoints/v2/main` plus every
  `refs/entire/checkpoints/v2/full/*` ref via `ensureLatestV2Refs`

These modes intentionally write local refs even when no migration data is
written. If the remote resolves, it must advertise both v1 and v2 /main; stale
local refs do not bypass a missing remote v1 or v2 /main. If no fetch target
can be resolved, the tool only proceeds when the required local refs already
exist. Otherwise it errors out before doing any analysis.

`--list` produces one line per checkpoint:
```text
<checkpoint-id> <commit-short-sha> [<commit-short-sha> ...]
<checkpoint-id> (orphan)
```
The first form is for commit-attributed IDs; the second is for orphans
(IDs present on v2 /main with no commit trailer in history). This is the
**universe** discovered — NOT the eligible set.

`plan`, `--dry-run`, and `--apply` produce:
```text
Migration plan:                          (or "Migration result:" on --apply)
  discovered checkpoints: D
  already present v1 sessions: A
  missing v2 checkpoint metadata: M1
  missing required v2 session metadata: M2
  missing raw transcripts: M3
  checkpoints eligible for migration: EC
  v2 orphan checkpoints eligible for migration: V2O
  sessions eligible for migration: ES
  migrated checkpoints: ...              (--apply only)
  migrated sessions: ...                 (--apply only)
  checkpoints to migrate:                (or "migrated checkpoint details:" on --apply)
    <id> sessions=N commits=<sha>[,<sha>...]
    <id> sessions=N commits=(orphan)
```

If `--since` or `--head` is set and the v2 /main ref carries IDs the scoped
trailer walk wouldn't have found, the tool prints a single line **before**
the report:
```text
warning: N v2 orphans skipped; re-run without --since/--head to include them
```

Checks that should always hold on the report:

- `EC ≤ D`.
- `V2O ≤ EC` (orphan-eligible is a subset of eligible).
- `ES ≥ EC` (each eligible checkpoint contributes ≥ 1 eligible session).
- `ES = Σ candidate.SessionCount`. The candidate list is exhaustive.
- The candidate list is sorted by `<id>` ascending; commit SHAs within
  a candidate are sorted by commit date descending (most recent first),
  ties broken by hash. Orphan candidates print `commits=(orphan)` instead
  of a SHA list.
- A `<id> sessions=N commits=(orphan)` line corresponds to one of the `V2O`
  checkpoints; its trailer never appears on any history tip included in the
  discovery walk.
- On `--apply`: `migrated checkpoints = EC` and `migrated sessions = ES` if
  no write errors. Anything less means a partial write failure — re-run
  the tool and the remainder should re-appear as eligible.
- Do not try to balance `D` with
  `eligible non-orphan + V2O + already-present + M1 + M3`. `D` is a
  checkpoint discovery count and includes both trailer-discovered and
  v2-orphan IDs; `A`, `M2`, and `M3` are session counters.
- `D = EC + (checkpoints with v2 summary but eligibleSessions==0) + M1`.
  The middle term covers both "all v2 sessions already in v1" and "every
  v2 session was unreadable (missing metadata or transcript)" — those land
  in the per-session counters `A`, `M2`, `M3` rather than dropping the
  checkpoint at the summary level.
- Counter sums for skipped sessions:
  `A + M2 + M3 = (Σ over all v2 sessions in checkpoints whose v2 summary
  exists) − ES`. Useful for spot-checking after `--apply`: if `A` is
  large and `EC` is small, most v2 checkpoints are already mirrored. If
  `V2O` is close to `EC` and `A` is small, this repo skipped v2 entirely
  and the migration is largely "import from v2 /main."

## 3. Validation procedure

The procedure below is the same regardless of repo. Substitute `$REPO` and
`$TOOL` per environment:

```sh
REPO=/path/to/some-repo                 # e.g. ~/entire/marvin
TOOL=~/entire/cli/.worktrees/review/migrate-v2-checkpoints
cd "$REPO"

sha256_stdin() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum | awk '{print $1}'
    else
        shasum -a 256 | awk '{print $1}'
    fi
}

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}
```

### 3.1 Pre-flight: confirm both stores exist

```sh
git -C "$REPO" show-ref refs/heads/entire/checkpoints/v1 \
    || git -C "$REPO" show-ref refs/remotes/origin/entire/checkpoints/v1
git -C "$REPO" show-ref refs/entire/checkpoints/v2/main
git -C "$REPO" show-ref refs/entire/checkpoints/v2/full/current
git -C "$REPO" for-each-ref 'refs/entire/checkpoints/v2/full/*' \
    --format='%(refname)'
```

If `entire/checkpoints/v1` is missing locally but present on the remote, the
migration tool will fetch it and create the local branch before planning or
applying. If both local and remote v1 are missing, the tool aborts; it will
not synthesize a fresh orphan v1 baseline for this rollback migration.

`plan`, `--dry-run`, and `--apply` auto-fetch checkpoint refs from the repo's
checkpoint remote before discovery (`ensureLatestV1Ref` and
`ensureLatestV2Refs`), so the local state *after* those modes runs will
reflect the remote. This is intentional even for `--dry-run`: the tool refuses
to analyze a stale checkpoint snapshot. If the remote lacks v1 or v2 /main, or
rejects a fetch of `refs/entire/checkpoints/v2/full/*`, the tool exits
non-zero before analysis. Ensure the repo can reach a checkpoint remote with
v1 and v2 refs before running. To work against a strictly local copy,
temporarily remove or disable the checkpoint fetch remote and keep local v1
and v2 refs present.

Pre-flight is still useful to sanity-check that the local v2 /main looks like
it's frozen rather than actively advancing. `--list` does **not** auto-fetch;
if you intend to inspect the universe via `--list`, pre-fetch manually (see
§9).

Also sanity-check the head of v2 isn't surprising — a recent commit
means v2 was being dual-written; a long-stale v2 head matches the
rollback narrative:

```sh
git -C "$REPO" log -1 --format='%h %ci %s' refs/entire/checkpoints/v2/main
```

### 3.2 Step A — sanity check the dry-run report

```sh
"$TOOL" --repo "$REPO" --dry-run | tee /tmp/migrate.plan
```

Spot-check the counter math against §2:

```sh
grep -E "^  (discovered|already|missing|checkpoints eligible|v2 orphan|sessions eligible)" \
    /tmp/migrate.plan
```

- `EC ≤ D` and `V2O ≤ EC` and `ES ≥ EC`.
- For each candidate line, parse `sessions=N` and sum — must equal `ES`.
- The number of candidate lines with `commits=(orphan)` must equal `V2O`.

```sh
awk '/^    [0-9a-f]{12} sessions=/ {sub(/sessions=/,"",$2); s+=$2} END {print s}' \
    /tmp/migrate.plan
# Should equal the "sessions eligible for migration" value.

grep -cE '^    [0-9a-f]{12} sessions=[0-9]+ commits=\(orphan\)$' /tmp/migrate.plan
# Should equal the "v2 orphan checkpoints eligible for migration" value.
```

If the run was launched with `--since` or `--head`, also confirm the
orphan-skip warning matches expectations:

```sh
grep "^warning: " /tmp/migrate.plan || echo "(no scope-orphan warning)"
```

### 3.3 Step B — confirm every candidate is genuinely v2-only-or-partial

For every candidate `<id>`:

```sh
ID=02d9783342a2     # example
SHARD=${ID:0:2}/${ID:2}
if git -C "$REPO" rev-parse --verify --quiet \
      refs/heads/entire/checkpoints/v1 >/dev/null; then
    V1_REF=refs/heads/entire/checkpoints/v1
else
    V1_REF=refs/remotes/origin/entire/checkpoints/v1
fi

# Does v2 /main carry this checkpoint?
git -C "$REPO" cat-file -p \
    refs/entire/checkpoints/v2/main:"$SHARD/metadata.json" \
    | jq '{checkpoint_id, sessions: [.sessions[].metadata]}'

# Does v1 already carry it? (Either the path doesn't exist, or the session
# IDs differ.)
git -C "$REPO" cat-file -p \
    "$V1_REF:$SHARD/metadata.json" 2>/dev/null \
    | jq '{checkpoint_id, sessions: [.sessions[].metadata]}' \
    || echo "(absent in v1)"
```

Use the **effective** v1 baseline the binary reads, not only the local branch.
The v1 store reads `refs/heads/entire/checkpoints/v1` first and falls back to
`refs/remotes/origin/entire/checkpoints/v1` if the local branch is missing.
This distinction matters before the tool has run in a fresh clone; after
`plan`/`--dry-run` or `--apply`, the v1 preflight should have created the
local branch from the remote baseline.

The candidate must satisfy at least one of:

1. `<SHARD>/metadata.json` doesn't exist on `entire/checkpoints/v1` →
   **fully v2-only**, all v2 sessions are eligible.
2. It exists on v1, but the v2 summary lists session IDs not present in v1 →
   **partial migration** to fill in missing sessions.

The reverse check — every v2 /main checkpoint should appear in the report
unless it's `already present` / `missing metadata` / `missing transcript`:

```sh
# Enumerate every checkpoint ID present on v2 /main (sharded layout).
git -C "$REPO" ls-tree -r refs/entire/checkpoints/v2/main \
    | awk '$4 ~ /metadata\.json$/ && $4 !~ /\// {next} \
           $4 ~ /^[0-9a-f]{2}\/[0-9a-f]{10}\/metadata\.json$/ {
               split($4, p, "/"); print p[1] p[2]
           }' \
    | sort -u > /tmp/v2_ids.txt
wc -l /tmp/v2_ids.txt

# IDs already in effective v1 (any session present).
if git -C "$REPO" rev-parse --verify --quiet \
      refs/heads/entire/checkpoints/v1 >/dev/null; then
    V1_REF=refs/heads/entire/checkpoints/v1
else
    V1_REF=refs/remotes/origin/entire/checkpoints/v1
fi
git -C "$REPO" ls-tree -r "$V1_REF" 2>/dev/null \
    | awk '$4 ~ /^[0-9a-f]{2}\/[0-9a-f]{10}\/metadata\.json$/ { \
               split($4, p, "/"); print p[1] p[2] \
           }' \
    | sort -u > /tmp/v1_ids.txt
comm -23 /tmp/v2_ids.txt /tmp/v1_ids.txt > /tmp/v2_only_ids.txt
wc -l /tmp/v2_only_ids.txt
```

Every ID in `v2_only_ids.txt` should be either a candidate or accounted for
by missing v2 checkpoint metadata, missing required v2 session metadata, or
missing raw transcript skips. Orphan candidates also live in this set: they
are exactly v2 /main IDs with no commit attribution but with intact v2
metadata + transcripts.

A quick predicate: the eligible candidate count plus the missing summary,
required-metadata, and missing-raw counters should equal or exceed the
v2-only set. If it's less, something is being silently dropped.

```sh
EC=$(grep "checkpoints eligible for migration" /tmp/migrate.plan | awk '{print $NF}')
V2O=$(grep "v2 orphan checkpoints" /tmp/migrate.plan | awk '{print $NF}')
M1=$(grep "missing v2 checkpoint metadata" /tmp/migrate.plan | awk '{print $NF}')
M2=$(grep "missing required v2 session metadata" /tmp/migrate.plan | awk '{print $NF}')
M3=$(grep "missing raw transcripts" /tmp/migrate.plan | awk '{print $NF}')
echo "v2-only on disk: $(wc -l < /tmp/v2_only_ids.txt)"
echo "EC=$EC  V2O=$V2O  M1=$M1  M2=$M2  M3=$M3"
echo "  EC + M1 + M2 + M3 must be >= v2-only count"
echo "  V2O <= EC must hold (orphan is a subset of eligible)"
```

(`>=` rather than `=` because `M1`, `M2`, and `M3` are counted over the
entire discovered universe, not only the v2-only set; `M2` and `M3` are
also per-session counters. `V2O` is exactly the subset of `EC` whose
discovery came from v2 /main alone.)

### 3.4 Step C — confirm commit-list accuracy

The report's `commits=...` are short SHAs of commits in history whose
message carries `Entire-Checkpoint: <id>`. Verify directly:

```sh
ID=02d9783342a2
git -C "$REPO" log --all --format='%h %s' --grep "Entire-Checkpoint: $ID"
```

The set of short SHAs that this prints should match the report's
`commits=…` for that ID. If they differ:

- `commits=(orphan)` in the report means the ID is on v2 /main but no
  reachable commit message carries its trailer. `git log --grep` should
  produce **no** output for that ID. If it does produce output, something
  is wrong — either the trailer walk dropped the commit or the orphan
  pass mislabelled the candidate.
- Extra in the report but absent here: the discovery walk picked up a tip
  this `--all` view doesn't include (rare).
- Extra here but absent in the report: a tip was filtered out
  (`entire/checkpoints/v1`, `entire/trails/v1`, or `*/HEAD` symbolic refs
  — see `isInternalHistoryRefName` / `isHistoryRef` in `history.go`).

A commit may also appear under multiple candidate IDs if it's a squash
merge with multiple trailers; that's expected.

### 3.5 Step D — DRY-RUN INSPECTION of session count

For each candidate, the report claims `sessions=N`. Confirm by counting v2
sessions that are not already in v1 **and** are eligible by the same filters
the migration applies: required metadata present and raw transcript present on
`/full/current` or an archived `/full/*` ref.

```sh
ID=02d9783342a2
SHARD=${ID:0:2}/${ID:2}
EXPECTED_SESSIONS=1   # report's sessions=N for this checkpoint

# Sessions advertised by the v2 summary (from /main).
V2_SESSION_COUNT=$(git -C "$REPO" cat-file -p \
    refs/entire/checkpoints/v2/main:"$SHARD/metadata.json" \
    | jq -r '.sessions | length')

# Session IDs already in v1 for this checkpoint.
if git -C "$REPO" cat-file -e \
      "entire/checkpoints/v1:$SHARD/metadata.json" 2>/dev/null; then
    V1_SESSION_COUNT=$(git -C "$REPO" cat-file -p \
        "entire/checkpoints/v1:$SHARD/metadata.json" \
        | jq -r '.sessions | length')
    for i in $(seq 0 $((V1_SESSION_COUNT-1))); do
        git -C "$REPO" cat-file -p \
            "entire/checkpoints/v1:$SHARD/$i/metadata.json" \
            | jq -r '.session_id'
    done | sort -u > /tmp/v1_sids.txt
else
    : > /tmp/v1_sids.txt
fi

FULL_REFS=$(git -C "$REPO" for-each-ref \
    --format='%(refname)' 'refs/entire/checkpoints/v2/full/*' \
  | awk '/full\/current$/ {print "1 " $0; next} {print "0 " $0}' \
  | sort -k1,1nr -k2,2r \
  | awk '{print $2}')

eligible=0
for i in $(seq 0 $((V2_SESSION_COUNT-1))); do
    META=$(git -C "$REPO" cat-file -p \
        refs/entire/checkpoints/v2/main:"$SHARD/$i/metadata.json" 2>/dev/null) \
        || continue

    SID=$(echo "$META" | jq -r '.session_id // ""')
    CPID=$(echo "$META" | jq -r '.checkpoint_id // ""')
    if [ -z "$SID" ] || [ -z "$CPID" ]; then
        continue
    fi
    if grep -qxF "$SID" /tmp/v1_sids.txt; then
        continue
    fi

    has_raw=0
    for r in $FULL_REFS; do
        if git -C "$REPO" cat-file -e \
              "$r:$SHARD/$i/raw_transcript" 2>/dev/null ||
           git -C "$REPO" ls-tree --name-only "$r:$SHARD/$i" 2>/dev/null \
              | grep -qE '^raw_transcript\.[0-9]{3}$'; then
            has_raw=1
            break
        fi
    done
    if [ "$has_raw" = 1 ]; then
        eligible=$((eligible+1))
    fi
done

echo "eligible sessions from v2: $eligible"
echo "report sessions=N:        $EXPECTED_SESSIONS"
[ "$eligible" -eq "$EXPECTED_SESSIONS" ] && echo OK || echo MISMATCH
```

Repeat for a random sample (5–10) across the candidate list. If your
sample matches 1:1, the report's accounting is trustworthy.

## 4. Apply the migration

> ⛔ **Human operator only. Agents must not run `--apply`.** If an agent is
> helping with this runbook, it may prepare commands, inspect dry-run output,
> update documentation, and analyze validation results, but it must stop before
> executing any command that includes `--apply`. The repository owner/operator
> runs the apply command manually in their own terminal and then shares the
> resulting report/output for follow-up validation.

> ⛔ **No `git push` for `entire/checkpoints/v1` from this point until §5
> has fully passed and the operator has consciously decided to publish.**
> The migration itself never pushes — but the v1 branch is the same ref
> any other tooling on the repo might push as part of its normal flow.
> Before running `--apply`:
>
> - confirm no automatic push hook, scheduler, or CI job will push
>   `refs/heads/entire/checkpoints/v1` in the background;
> - if `entire`'s own push path runs in this repo (e.g. on the next
>   `entire`-driven commit), pause it until §5 is done;
> - if the repo has `checkpoint_remote` configured, treat that as another
>   push target that must stay quiet.
>
> Pushing before §5 passes means a bad migration is now everyone else's
> problem. Pushing after §5 passes is a separate, manual procedure that
> lives outside this runbook.

**This is the destructive (local) step.** Up to here everything was
read-only. Now we write new commits to the local
`entire/checkpoints/v1` branch. Nothing is pushed to any remote — that's
a separate, explicit decision once the post-apply checks in §5 pass.

### Preconditions

- §3 ran clean: the candidate list looks plausible, counter math adds up,
  and a spot sample (Steps C and D) confirmed the candidates really are
  v2-only / partial migrations.
- The repo has either a checkpoint fetch remote that advertises both
  `refs/heads/entire/checkpoints/v1` and `refs/entire/checkpoints/v2/main`,
  or no resolvable fetch remote but already-present local v1 and v2 /main
  refs. `--apply` calls `ensureLatestV1Ref` and `ensureLatestV2Refs` first;
  it refreshes the local v1 branch, v2 /main, and every
  `refs/entire/checkpoints/v2/full/*` ref from the remote (forced fetch of
  v2 `/full/current`, fast-forward fetch of archives). If the fetch target
  resolves but lacks v1 or v2 /main, the tool errors out even if a stale
  local ref exists; that prevents silently using stale rollback data. A
  manual pre-fetch is no longer required, but remains a safe no-op:

  ```sh
  git -C "$REPO" fetch origin \
      'refs/heads/entire/checkpoints/v1:refs/heads/entire/checkpoints/v1' \
      --no-tags
  git -C "$REPO" fetch origin \
      'refs/entire/checkpoints/v2/*:refs/entire/checkpoints/v2/*' \
      --no-tags
  ```

- Working tree is clean OR you don't mind running with uncommitted changes
  in `$REPO`. The tool only touches refs, not the working tree, but a clean
  tree makes it easier to roll back if needed.

### Recommended invocation

```sh
REPO=/path/to/some-repo
REPO_NAME=$(basename "$REPO")
TOOL=~/entire/cli/.worktrees/review/migrate-v2-checkpoints
APPLIED_REPORT="/tmp/migrate-${REPO_NAME}.applied"

# Snapshot the v1 branch tip so you can roll back deterministically.
PRE_APPLY_TIP=$(git -C "$REPO" rev-parse entire/checkpoints/v1 2>/dev/null || echo "none")
echo "pre-apply v1 tip: $PRE_APPLY_TIP"

# USER ONLY: an agent must not execute this command.
# Apply. Tee the report into /tmp/migrate-${REPO_NAME}.applied — §5 reads it back.
"$TOOL" --repo "$REPO" --apply | tee "$APPLIED_REPORT"

# Sanity-check the report.
grep -E "^  (checkpoints eligible|v2 orphan|sessions eligible|migrated)" "$APPLIED_REPORT"
#   migrated checkpoints == checkpoints eligible
#   migrated sessions    == sessions eligible
#   v2 orphan ...        == subset of EC (informational; not a pass/fail gate)
# Anything less means at least one write failed silently — re-run --apply
# (idempotent) and inspect logs.

# Confirm the v1 branch actually advanced.
POST_APPLY_TIP=$(git -C "$REPO" rev-parse entire/checkpoints/v1)
echo "post-apply v1 tip: $POST_APPLY_TIP"
git -C "$REPO" log --format='%h %ci %s' \
    "$PRE_APPLY_TIP".."$POST_APPLY_TIP" 2>/dev/null \
    | head -20
```

### Behavior notes

- **Idempotent.** Re-running `--apply` after a successful apply yields
  `checkpoints eligible for migration: 0` (and re-runs are cheap). Safe
  to retry on partial failure.
- **Local only — and stays local for the rest of this runbook.** No
  remotes are touched by `--apply` itself. The new v1 commits live on
  `refs/heads/entire/checkpoints/v1` locally. **Do not** `git push` this
  branch, do not let `entire`'s push path publish it, do not let any
  CI/hook/scheduler publish it, and do not let a configured
  `checkpoint_remote` mirror it. Push is a separate manual procedure
  that is explicitly out of scope here, and is only safe **after** every
  step in §5 passes and the operator is satisfied.
- **Per-session atomicity, not transactional.** Each migrated session is
  written as its own commit on v1. If `--apply` errors out partway
  through a checkpoint with multiple eligible sessions, earlier sessions
  remain written and later sessions reappear on the next run.
- **v1 commit author matches v2.** Each new v1 commit is authored with
  the same name, email, and author timestamp as the v2 `/main` non-merge
  commit that actually changed the migrated session's `metadata.json`.
  Later commits that merely carry that path through their tree do not
  count. §5.6 treats the `author` header in `entire explain` as a required
  check; a mismatch is a regression, not an accepted divergence.
- **Migration commits are unsigned.** The tool disables checkpoint commit
  signing for migrated writes, even if normal checkpoint signing is enabled
  in the repo. The v1 author line is replayed from v2 history; adding a local
  operator signature to that replayed author would be misleading. Any signed
  migrated v1 commit is a bug.
- **Roll back** by resetting v1 back to `$PRE_APPLY_TIP`:

  ```sh
  # Only if you need to undo — this discards the new commits locally.
  if [ "$PRE_APPLY_TIP" = "none" ]; then
      git -C "$REPO" update-ref -d refs/heads/entire/checkpoints/v1
  else
      git -C "$REPO" update-ref refs/heads/entire/checkpoints/v1 "$PRE_APPLY_TIP"
  fi
  ```

  Safe before any push. Destructive after push.

### Operator checkpoint

**Stop here. If you are an agent, do not run `--apply`. The human operator
must run the apply command themselves and confirm:**

1. `migrated checkpoints` equals `checkpoints eligible for migration` from
   the dry-run.
2. `migrated sessions` equals `sessions eligible for migration` from the
   dry-run.
3. `git rev-parse entire/checkpoints/v1` advanced.
4. `/tmp/migrate-${REPO_NAME}.applied` contains the full report for §5 to reference.
5. **You have NOT pushed `entire/checkpoints/v1`.** Confirm by checking
   that no remote tracking ref has advanced:

   ```sh
   git -C "$REPO" for-each-ref \
       --format='%(refname) %(objectname:short)' \
       'refs/remotes/*/entire/checkpoints/v1'
   ```

   Each remote ref should still point at the pre-apply tip (or be
   absent). If a remote ref has already moved to the new local tip,
   pause and figure out who pushed — do not proceed to §5 until you've
   understood the source of the push and decided whether to roll back.

Then proceed to §5. Do not push between §4 and §5; do not push during
§5; do not push without the operator's explicit go-ahead after §5
passes.

## 5. Post-apply validation

This section assumes `--apply` has been run and
`/tmp/migrate-${REPO_NAME}.applied` holds the report. The
`migrated sessions=...` count is the population you will validate below.
Extract the migrated checkpoint IDs once and reuse that list for every
bulk check:

```sh
MIGRATED_IDS="/tmp/migrate-${REPO_NAME}.migrated-checkpoints"
awk '/^    [0-9a-f]{12} sessions=/ {print $1}' "$APPLIED_REPORT" \
  > "$MIGRATED_IDS"
wc -l "$MIGRATED_IDS"
```

### 5.1 Step E — root `metadata.json` (CheckpointSummary) on v1

For each migrated checkpoint, decode the v1 root metadata and confirm:

```sh
ID=02d9783342a2
SHARD=${ID:0:2}/${ID:2}

git -C "$REPO" cat-file -p "entire/checkpoints/v1:$SHARD/metadata.json" | jq .
```

Expected shape (schema lives at
`cmd/entire/cli/checkpoint/checkpoint.go:545-563`):

```jsonc
{
  "cli_version": "…",            // optional
  "checkpoint_id": "02d9783342a2",
  "strategy": "manual-commit",
  "branch": "main",              // optional
  "checkpoints_count": 1,
  "files_touched": ["…"],
  "sessions": [
    {
      "metadata":     "/02/d9783342a2/0/metadata.json",
      "transcript":   "/02/d9783342a2/0/full.jsonl",     // omitempty
      "content_hash": "/02/d9783342a2/0/content_hash.txt", // omitempty
      "prompt":       "/02/d9783342a2/0/prompt.txt"
    }
  ],
  "token_usage": { … },          // omitempty fields
  "combined_attribution": { … },
  "has_review": true             // omitempty
}
```

For checkpoints that were fully v2-only and whose v2 sessions all migrated,
the root summary should match the v2 summary for the stable fields below:

```sh
diff <(git -C "$REPO" cat-file -p \
        refs/entire/checkpoints/v2/main:"$SHARD/metadata.json" \
        | jq '{checkpoint_id, strategy, branch, checkpoints_count,
               files_touched, combined_attribution, has_review,
               token_usage}') \
     <(git -C "$REPO" cat-file -p \
        entire/checkpoints/v1:"$SHARD/metadata.json" \
        | jq '{checkpoint_id, strategy, branch, checkpoints_count,
               files_touched, combined_attribution, has_review,
               token_usage}')
```

Acceptable differences:

- `sessions[]` entries differ — paths point to v1 file names
  (`full.jsonl`, `content_hash.txt`), not v2's compact format.
- If v1 already had sessions, `sessions[]` length on v1 may exceed v2's;
  the candidate's contributions are appended.
- If only some v2 sessions migrated (because others were already present,
  lacked required metadata, or lacked raw transcripts), aggregate fields
  such as `checkpoints_count`, `files_touched`, `token_usage`, and
  `has_review` may differ. The v1 writer reaggregates those fields from
  the sessions actually present in v1, not from every session in v2.
- `combined_attribution` may also differ when v1 already had sessions. For
  purely v2-only checkpoints with all v2 sessions migrated, it should match
  the v2 summary exactly because the migration uses
  `summary.CombinedAttribution` from v2 verbatim (`migration.go:242`).

Hard requirements:

- `checkpoint_id` equals the directory shard.
- `sessions[].metadata`, `sessions[].transcript` (if non-empty),
  `sessions[].content_hash` (if non-empty), `sessions[].prompt` all start
  with `/<shard>/<N>/` and end with the correct filename constants.

### 5.2 Step F — per-session `metadata.json`

For each migrated session, locate it by `session_id` rather than by index:

```sh
ID=02d9783342a2
SHARD=${ID:0:2}/${ID:2}
WANT_SID=…  # session_id from the v2 side

V1_SUM=$(git -C "$REPO" cat-file -p "entire/checkpoints/v1:$SHARD/metadata.json")
V1_LEN=$(echo "$V1_SUM" | jq '.sessions | length')
V1_SLOT=
for n in $(seq 0 $((V1_LEN-1))); do
    SID=$(git -C "$REPO" cat-file -p \
            "entire/checkpoints/v1:$SHARD/$n/metadata.json" \
          | jq -r '.session_id')
    if [ "$SID" = "$WANT_SID" ]; then
        V1_SLOT=$n; break
    fi
done
if [ -n "$V1_SLOT" ]; then
    echo "session $WANT_SID lives in v1 slot $V1_SLOT"
else
    echo "session $WANT_SID is absent from v1"
fi
```

If a v2 `session_id` is present in v2 /main but absent from v1 after apply,
check that it was not skipped because v2 no longer has a raw transcript. Those
sessions are counted in `missing raw transcripts` and are intentionally not
written to v1.

```sh
V2_SLOT=…   # slot the session occupied on v2 (its index in v2 summary)
if [ -z "$V1_SLOT" ]; then
    V2_FULL_REFS=$(git -C "$REPO" for-each-ref \
        --format='%(refname)' 'refs/entire/checkpoints/v2/full/*' \
      | awk '/full\/current$/ {print "1 " $0; next} {print "0 " $0}' \
      | sort -k1,1nr -k2,2r \
      | awk '{print $2}')

    RAW_FOUND=
    for r in $V2_FULL_REFS; do
        if git -C "$REPO" cat-file -e \
              "$r:$SHARD/$V2_SLOT/raw_transcript" 2>/dev/null ||
           git -C "$REPO" ls-tree --name-only "$r:$SHARD/$V2_SLOT" 2>/dev/null \
              | grep -qE '^raw_transcript\.[0-9]{3}$'; then
            RAW_FOUND=1
            break
        fi
    done

    if [ -z "$RAW_FOUND" ]; then
        echo "session $WANT_SID absent in v1: M3 skip, expected"
    else
        echo "MISMATCH: session $WANT_SID has raw v2 transcript but is absent in v1"
    fi
fi
```

When `V1_SLOT` is non-empty, diff the per-session metadata, comparing
**fields that are expected to survive migration** (`migration.go:216-248`
lists them explicitly):

```sh
V2_SLOT=…   # slot the session occupied on v2 (its index in v2 summary)

diff <(git -C "$REPO" cat-file -p \
        refs/entire/checkpoints/v2/main:"$SHARD/$V2_SLOT/metadata.json" \
        | jq '{checkpoint_id, session_id, strategy, branch,
               files_touched, checkpoints_count, agent, model,
               turn_id, is_task, tool_use_id,
               transcript_identifier_at_start,
               checkpoint_transcript_start,
               token_usage, session_metrics,
               initial_attribution, prompt_attributions,
               summary, kind, review_skills, review_prompt}') \
     <(git -C "$REPO" cat-file -p \
        entire/checkpoints/v1:"$SHARD/$V1_SLOT/metadata.json" \
        | jq '{checkpoint_id, session_id, strategy, branch,
               files_touched, checkpoints_count, agent, model,
               turn_id, is_task, tool_use_id,
               transcript_identifier_at_start,
               checkpoint_transcript_start,
               token_usage, session_metrics,
               initial_attribution, prompt_attributions,
               summary, kind, review_skills, review_prompt}')
```

Expected: no diff. Special cases:

- `created_at` is replayed from v2's `created_at` into the v1 metadata JSON.
  The v1 metadata-branch commit timestamp is a separate git author timestamp
  copied from the v2 `/main` commit that last touched this session's
  `metadata.json`.
- The migration sets `HasReview = session.Kind(meta.Kind).IsReview()`
  (`migration.go:247`). For non-review kinds this is `false` and may
  have been absent (omitempty) in v2; that's still a match.
- `cli_version` on the v1 session may differ from v2's. The migration
  doesn't pass `CLIVersion`, so v1 inherits whatever default the writer
  applies — generally an empty value or the current binary's version. Not
  a correctness issue.
- Root summary aggregation is covered in §5.1. Session-level comparison here
  should ignore root-only fields such as `combined_attribution`; per-session
  `token_usage` is copied from v2 and then folded into the root summary by
  the v1 writer.

Schema sanity per session:

```sh
git -C "$REPO" cat-file -p "entire/checkpoints/v1:$SHARD/$V1_SLOT/metadata.json" \
  | jq -e 'has("checkpoint_id") and has("session_id") and has("created_at")' \
  > /dev/null && echo OK
```

Author and signature status for the metadata-branch commit:

```sh
V2_AUTHOR=$(git -C "$REPO" log -1 --format='%an <%ae> %aI' \
    refs/entire/checkpoints/v2/main -- "$SHARD/$V2_SLOT/metadata.json")
V1_AUTHOR=$(git -C "$REPO" log -1 --format='%an <%ae> %aI' \
    entire/checkpoints/v1 -- "$SHARD/$V1_SLOT/metadata.json")
V1_SIGNATURE_STATUS=$(git -C "$REPO" log -1 --format='%G?' \
    entire/checkpoints/v1 -- "$SHARD/$V1_SLOT/metadata.json")

echo "v2: $V2_AUTHOR"
echo "v1: $V1_AUTHOR"
[ "$V1_AUTHOR" = "$V2_AUTHOR" ] && echo OK || echo MISMATCH

echo "v1 signature status: $V1_SIGNATURE_STATUS"
[ "$V1_SIGNATURE_STATUS" = "N" ] && echo OK || echo MISMATCH
```

Expected: exact author match, and v1 signature status `N` (`%G? = N` means
no signature). For orphan candidates the author check is still valid: the v2
`/main` path history is the source of the author line even though no user
commit trailer exists.

### 5.3 Step G — `prompt.txt` content

The migration joins v2 prompts (split form on disk) back into a single
`prompt.txt` via `SplitPromptContent` round-trip. If `prompt.txt` exists on
v2, the v1 bytes should match. If it is absent on v2, it should also be
absent on v1.

```sh
if git -C "$REPO" cat-file -e \
      "refs/entire/checkpoints/v2/main:$SHARD/$V2_SLOT/prompt.txt" 2>/dev/null; then
    V2_PROMPT_HASH=$(git -C "$REPO" cat-file -p \
        "refs/entire/checkpoints/v2/main:$SHARD/$V2_SLOT/prompt.txt" \
        | sha256_stdin)
    V1_PROMPT_HASH=$(git -C "$REPO" cat-file -p \
        "entire/checkpoints/v1:$SHARD/$V1_SLOT/prompt.txt" \
        | sha256_stdin)
    echo "v2 prompt: $V2_PROMPT_HASH"
    echo "v1 prompt: $V1_PROMPT_HASH"
    [ "$V1_PROMPT_HASH" = "$V2_PROMPT_HASH" ] && echo OK || echo MISMATCH
else
    git -C "$REPO" cat-file -e \
        "entire/checkpoints/v1:$SHARD/$V1_SLOT/prompt.txt" 2>/dev/null \
        && echo "MISMATCH: v1 prompt exists but v2 prompt is absent" \
        || echo "OK: prompt absent in both stores"
fi
```

If the digests don't match, inspect with a `diff -u` between the two
`cat-file -p` outputs to see whether it's an ordering / separator issue.

### 5.4 Step H — raw transcript & `content_hash.txt`

This is the most important check. Two layers:

1. **Self-consistency on v1**: the value in `content_hash.txt` must equal
   `sha256:<hex>` of the reassembled `full.jsonl[.NNN]` content.
2. **Cross-store match (non-Codex agents)**: reassembled v1 bytes should
   equal reassembled v2 `raw_transcript[.NNN]` bytes, and v1's
   `content_hash.txt` should equal v2's `raw_transcript_hash.txt`.

Reassemble logic: ordered list `full.jsonl`, `full.jsonl.001`,
`full.jsonl.002`, … For most agents this is JSONL with `\n` separators
between chunks (`agent.ReassembleJSONL` in `agent/chunking.go:109-118`);
for `vogon`, OpenCode etc. the agent's own `ReassembleTranscript` is
used at read time. For validation, byte-concatenation in chunk order is
what the v1 writer hashed (`committed.go:784` — the hash is over
`transcriptBytes` BEFORE chunking), so the easier check is to read the
original v1 input bytes back via the v1 store API, OR to validate that
each chunk blob is what the v1 writer would have produced.

The simplest robust shell check: reconstruct via ordered concat and
compute the digest, then compare to `content_hash.txt`. This is exact for
agents whose `ChunkTranscript` is a byte-preserving JSONL chunker
(Claude Code, Gemini CLI, Cursor, Copilot CLI, Codex except for the pre-
chunk sanitization step, and the generic case). It's slightly fuzzy for
agents whose chunking strips/reflows bytes — but in practice the round
trip is byte-exact for the supported set.

```sh
ID=02d9783342a2; SHARD=${ID:0:2}/${ID:2}; V1_SLOT=0

# Enumerate transcript chunks in order.
git -C "$REPO" ls-tree --name-only \
    "entire/checkpoints/v1:$SHARD/$V1_SLOT" \
  | grep -E '^full\.jsonl(\.[0-9]{3})?$' \
  | sort > /tmp/chunks.txt
cat /tmp/chunks.txt

# Concatenate chunks (no extra separator — chunk files are written as
# they will be read by the agent's reassembler). For JSONL agents,
# the writer already trimmed the trailing newline per chunk; the
# reassembler joins with "\n". Reproduce that here.
tmp=$(mktemp)
first=1
while IFS= read -r f; do
    if [ $first -eq 0 ]; then printf '\n' >> "$tmp"; fi
    git -C "$REPO" cat-file -p \
        "entire/checkpoints/v1:$SHARD/$V1_SLOT/$f" >> "$tmp"
    first=0
done < /tmp/chunks.txt

# Recompute and compare.
COMPUTED="sha256:$(sha256_file "$tmp")"
STORED=$(git -C "$REPO" cat-file -p \
            "entire/checkpoints/v1:$SHARD/$V1_SLOT/content_hash.txt")
echo "stored:   $STORED"
echo "computed: $COMPUTED"
[ "$STORED" = "$COMPUTED" ] && echo OK || echo MISMATCH
```

If `STORED ≠ COMPUTED` for **JSONL-based agents** (Claude Code, Gemini
CLI, etc.), something is wrong with the migration — flag it. For agents
with custom chunkers the shell heuristic above can produce a false
mismatch; in those cases fall back to using the CLI's own reader by
running `entire checkpoint explain <id>` or, more directly, by writing
a small Go probe that calls `agent.ReassembleTranscript(chunks, agent)`
and re-hashes the result.

Cross-store comparison (non-Codex):

```sh
# Same /full ref resolution as the migration (current first, then archives).
FULL_REFS=$(git -C "$REPO" for-each-ref \
    --format='%(refname)' 'refs/entire/checkpoints/v2/full/*' \
  | awk '/full\/current$/ {print "1 " $0; next} {print "0 " $0}' \
  | sort -k1,1nr -k2,2r \
  | awk '{print $2}')
RAW_HASH=""
for r in $FULL_REFS; do
    if git -C "$REPO" cat-file -e \
          "$r:$SHARD/$V2_SLOT/raw_transcript_hash.txt" 2>/dev/null; then
        RAW_HASH=$(git -C "$REPO" cat-file -p \
                    "$r:$SHARD/$V2_SLOT/raw_transcript_hash.txt")
        echo "raw transcript found on $r: $RAW_HASH"
        break
    fi
done
echo "v1 content_hash:    $STORED"
echo "v2 raw_transcript:  $RAW_HASH"
```

For non-Codex agents, the two hashes should match. For Codex (agent
field on the session metadata is `codex`), they are allowed to differ —
v1 sanitizes via `codex.SanitizePortableTranscript` before hashing
(`committed.go:746`). The v1 self-consistency check above is still
required in that case.

### 5.5 Step I — bulk sweep

Once the per-checkpoint procedure is established, sweep every migrated
checkpoint:

```sh
wc -l "$MIGRATED_IDS"
```

Then for each ID in `$MIGRATED_IDS`, run:

- §5.1 root metadata diff (`grep -q` for errors).
- §5.2 per-session field diff for every session ID that the checkpoint
  brought in.
- §5.3 prompt presence/content check for every migrated session.
- §5.4 hash check on every transcript chunk set.
- §5.6 `entire explain` comparison against the dual-reads-removed binary.

A single shell loop is fine, and the validation completes in seconds per
checkpoint. Surface any non-empty diffs or any `MISMATCH` lines.

### 5.6 Step J — `entire explain` parity after removing v2 dual reads

Run a current build from the branch that removes v2-first dual reads and
compare it to the Homebrew-installed `entire` for every migrated checkpoint.
On a real repo the two binaries will **not** produce identical output —
some divergence is structural and expected. The gate here is that every
diff falls into a known, bounded category and the required checks hold: the
file list, displayed session id, author header, and exit status agree on
every checkpoint.

#### Expected divergences

1. **Codex sessions: sanitized transcript on v1** (§1.3).
   `codex.SanitizePortableTranscript` runs at write time, so the v1
   transcript bytes are not equal to v2's raw transcript bytes for any
   session with `agent == "Codex"`. Self-consistency on each side still
   holds — verified in §5.4.
2. **v2 compact transcript not migrated** (§7). The v2 store holds two
   transcripts per session: the raw form on `/full/*` (migrated to v1) and
   the compact form on `/main/transcript.jsonl` (not migrated). BREW
   renders explain output from the compact form when available; FIX has to
   parse the raw JSONL on v1 and pick fields ad hoc. Two visible
   consequences:
   - For Claude Code multi-argument tools (`Glob`, `Grep`, …), the tool
     summary line picks different arguments. BREW tends to surface
     `path`; FIX tends to surface `pattern`. Both arguments are present
     in the raw JSONL.
   - For the **Intent** block, BREW shows a prompt derived from the
     compact transcript; FIX picks a user message from the raw
     transcript. The underlying `prompt.txt` blobs are byte-identical
     between v1 and v2 — only the renderer's selection differs.

#### Required checks that must still hold

- Exit status of both binaries matches per checkpoint.
- `## Files` (the touched-files list) is byte-identical.
- The displayed `session ` header line is identical (both binaries
  choose the same session id on both sides).
- The `author` header is identical (the migration preserves the v2
  author identity onto v1 — see §4 Behavior notes).

Anything that violates one of those is **unaccounted for** — flag it.

Build the comparison binary immediately before the sweep:

```sh
FIX_WORKTREE=/Users/pfleidi/entire/cli/.worktrees/fix/checkpoints-v2-remove-dual-reads
FIX_ENTIRE="/tmp/entire-${REPO_NAME}-remove-dual-reads"
BREW_ENTIRE="$(brew --prefix)/bin/entire"

git -C "$FIX_WORKTREE" status --short --branch
(cd "$FIX_WORKTREE" && go build -o "$FIX_ENTIRE" ./cmd/entire)

"$FIX_ENTIRE" version
"$BREW_ENTIRE" version
```

Run both binaries from the migrated repo for every migrated checkpoint and
audit each diff against the required checks above:

```sh
EXPLAIN_DIR="/tmp/migrate-${REPO_NAME}-explain"
mkdir -p "$EXPLAIN_DIR"
: > "$EXPLAIN_DIR/unaccounted.txt"
set +e

while IFS= read -r ID; do
    FIX_RESULT="$EXPLAIN_DIR/$ID.fix"
    BREW_RESULT="$EXPLAIN_DIR/$ID.brew"
    DIFF_FILE="$EXPLAIN_DIR/$ID.diff"

    (cd "$REPO" && "$FIX_ENTIRE" explain "$ID") \
        > "$FIX_RESULT.out" 2> "$FIX_RESULT.err"
    FIX_STATUS=$?
    (cd "$REPO" && "$BREW_ENTIRE" explain "$ID") \
        > "$BREW_RESULT.out" 2> "$BREW_RESULT.err"
    BREW_STATUS=$?

    {
        echo "status=$FIX_STATUS"
        cat "$FIX_RESULT.out"
        printf '\n--- stderr ---\n'
        cat "$FIX_RESULT.err"
    } > "$FIX_RESULT"
    {
        echo "status=$BREW_STATUS"
        cat "$BREW_RESULT.out"
        printf '\n--- stderr ---\n'
        cat "$BREW_RESULT.err"
    } > "$BREW_RESULT"

    if diff -u "$BREW_RESULT" "$FIX_RESULT" > "$DIFF_FILE" 2>&1; then
        rm -f "$DIFF_FILE"
        continue
    fi

    # --- Required checks ---------------------------------------------------
    if [ "$FIX_STATUS" != "$BREW_STATUS" ]; then
        echo "$ID reason=exit-status brew=$BREW_STATUS fix=$FIX_STATUS" \
            >> "$EXPLAIN_DIR/unaccounted.txt"
        continue
    fi
    BREW_FILES=$(awk '/^## Files/{f=1} /^── Transcript/{f=0} f' \
                   "$BREW_RESULT" | sha256_stdin)
    FIX_FILES=$(awk '/^## Files/{f=1} /^── Transcript/{f=0} f' \
                  "$FIX_RESULT" | sha256_stdin)
    if [ "$BREW_FILES" != "$FIX_FILES" ]; then
        echo "$ID reason=files-list-diverges" \
            >> "$EXPLAIN_DIR/unaccounted.txt"
        continue
    fi
    BREW_SID=$(awk '/^  session /{print $2; exit}' "$BREW_RESULT")
    FIX_SID=$(awk '/^  session /{print $2; exit}' "$FIX_RESULT")
    if [ "$BREW_SID" != "$FIX_SID" ]; then
        echo "$ID reason=session-id-mismatch brew=$BREW_SID fix=$FIX_SID" \
            >> "$EXPLAIN_DIR/unaccounted.txt"
        continue
    fi
    BREW_AUTHOR=$(grep -m1 '^  author ' "$BREW_RESULT")
    FIX_AUTHOR=$(grep -m1 '^  author ' "$FIX_RESULT")
    if [ "$BREW_AUTHOR" != "$FIX_AUTHOR" ]; then
        echo "$ID reason=author-mismatch" \
            >> "$EXPLAIN_DIR/unaccounted.txt"
        continue
    fi
    # Remaining diffs fall in the expected buckets above. Leave $DIFF_FILE
    # on disk for spot-checking.
done < "$MIGRATED_IDS"

if [ -s "$EXPLAIN_DIR/unaccounted.txt" ]; then
    echo "unaccounted-for explain divergences:"
    cat "$EXPLAIN_DIR/unaccounted.txt"
    exit 1
fi

echo "all explain divergences fall in accepted buckets"
```

#### Optional: divergence distribution

Useful for spotting a sudden shift in divergence shape between releases.
Bucket each checkpoint as `identical`, `body-with-codex` (sanitization +
compact-rendering), or `body-without-codex` (compact-rendering only):

```sh
identical=0; codex_body=0; non_codex_body=0
while IFS= read -r ID; do
    DIFF="$EXPLAIN_DIR/$ID.diff"
    if [ ! -f "$DIFF" ]; then
        identical=$((identical+1)); continue
    fi
    SHARD=${ID:0:2}/${ID:2}
    has_codex=0
    V1_LEN=$(git -C "$REPO" cat-file -p \
                "entire/checkpoints/v1:$SHARD/metadata.json" \
              | jq -r '.sessions | length')
    for i in $(seq 0 $((V1_LEN-1))); do
        AGENT=$(git -C "$REPO" cat-file -p \
                  "entire/checkpoints/v1:$SHARD/$i/metadata.json" \
                | jq -r '.agent // ""')
        if [ "$AGENT" = "Codex" ] || [ "$AGENT" = "codex" ]; then
            has_codex=1; break
        fi
    done
    if [ "$has_codex" = 1 ]; then
        codex_body=$((codex_body+1))
    else
        non_codex_body=$((non_codex_body+1))
    fi
done < "$MIGRATED_IDS"

printf 'identical:            %4d\n' "$identical"
printf 'body diff, has codex: %4d  (expected: §1.3 sanitization + §7 compact-not-migrated)\n' \
    "$codex_body"
printf 'body diff, no codex:  %4d  (expected: §7 compact-not-migrated)\n' \
    "$non_codex_body"
```

`$EXPLAIN_DIR/*.diff`, `*.fix`, and `*.brew` are kept on disk for
inspection. If a body diff in the no-codex bucket touches anything other
than transcript-tool-argument rendering or Intent text, file a bug — that
would be a real read-path regression.

### 5.7 After validation passes

You're done with this runbook only after every step in §5 produced the
expected result on every migrated checkpoint. Publishing the migration is **out of
scope for this runbook** and explicitly a manual decision.

When the operator is satisfied and ready to publish:

1. Re-read §4's push warning. Nothing about it has changed.
2. Decide deliberately, out-of-band, that you want the new v1 commits on
   the remote. Coordinate with anyone else who has the repo cloned —
   they will pick up the new commits on their next fetch.
3. Use your repo's normal push path. The runbook does not prescribe one
   because publishing semantics vary per repo (some use `entire`'s push
   integration, some use `checkpoint_remote`, some do a plain
   `git push`). Pick the right one explicitly.

Until that conscious decision is made, `entire/checkpoints/v1` stays
local. If §5 surfaces a problem, roll back with the `update-ref` snippet
from §4's "Behavior notes" — cheap and local, because you did not push.

## 6. Failure modes and what they mean

| Symptom in dry-run                                             | Meaning                                                                                                             | Action                                                                                  |
|----------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------|
| `missing v2 checkpoint metadata: N (large)`                    | v2 `/main` is missing or its tree lacks summaries for many discovered IDs.                                          | Confirm `refs/entire/checkpoints/v2/main` exists, was fetched, and is reasonably recent. |
| `missing required v2 session metadata: > 0`                    | v2 session `metadata.json` lacks `checkpoint_id` or `session_id`. Could indicate corruption or a partial v2 write.  | Inspect the affected sessions manually; they will be skipped, not failed.               |
| `missing raw transcripts: > 0`                                 | v2 `/main` has a session but `/full/current` and archived `/full/*` don't carry its `raw_transcript*` data.         | Confirm archived `/full/*` refs are present locally (or accessible via remote fetch).   |
| Candidate `commits=(orphan)`                                   | The ID is on v2 /main with no commit-trailer attribution in history. Expected and benign; counted by `V2O`.         | None — verify against `git log --grep` in §3.4 to confirm there's no missed trailer.    |
| `warning: N v2 orphans skipped` on a `--since`/`--head` run    | Commit-scoped run found N v2 /main IDs that an unscoped walk would have surfaced as orphan candidates.              | Re-run without `--since`/`--head` to include them, or accept the scope deliberately.    |
| `v2 orphan checkpoints eligible for migration > checkpoints eligible for migration` | Should be impossible (`V2O ⊆ EC` by construction).                                             | File a bug.                                                                             |
| `sessions=N` for a candidate doesn't match the §3.5 expected   | Either v1 already has the session (so report should have lower N), or session IDs are non-unique within v2.        | Inspect; non-unique session IDs are a v2 corruption.                                    |
| Post-apply, `content_hash.txt` ≠ recomputed SHA-256            | Codex agent + ours-vs-original sanitization difference, OR a bug. Confirm `agent` field on the session.            | If non-Codex, file a bug with chunk listing + bytes.                                    |
| Post-apply, `content_hash.txt` matches but v2's `raw_transcript_hash.txt` doesn't | Codex sanitization (expected) OR transcript was rewritten in transit. Confirm agent first.               | If non-Codex, file a bug.                                                               |
| Post-apply, migrated v1 commit has signature status other than `N` | The migration signed a replayed-author commit. This should not happen.                                      | File a bug and do not publish the migrated v1 branch until re-run with an unsigned tool. |
| Re-running `--dry-run` after `--apply` still lists the same candidates | Apply failed silently or didn't get pushed before re-fetch. Look at the `migrated sessions` count.         | Re-run with verbose logging; check that v1 branch actually advanced.                    |

The report does not enumerate the exact checkpoint/session IDs behind `M1`,
`M2`, or `M3`. Manual inspection requires re-walking v2 /main and v2 /full
refs as shown in §3.3 and §3.5.

## 7. Quick reference: file & ref constants

| Concept                  | Constant                                       | Value                                              | Source                                |
|--------------------------|------------------------------------------------|----------------------------------------------------|---------------------------------------|
| v1 branch                | `paths.MetadataBranchName`                     | `entire/checkpoints/v1` (under `refs/heads/`)      | `paths/paths.go:43`                   |
| v2 main ref              | `paths.V2MainRefName`                          | `refs/entire/checkpoints/v2/main`                  | `paths/paths.go:49`                   |
| v2 full current ref      | `paths.V2FullCurrentRefName`                   | `refs/entire/checkpoints/v2/full/current`          | `paths/paths.go:52`                   |
| v2 archived full ref     | (pattern)                                      | `refs/entire/checkpoints/v2/full/<13-digit-suffix>`| `v2_read.go:523-533`                  |
| Root summary             | `paths.MetadataFileName`                       | `metadata.json`                                    | `paths/paths.go:36`                   |
| Session metadata         | `paths.MetadataFileName`                       | `metadata.json`                                    | `paths/paths.go:36`                   |
| Session prompt           | `paths.PromptFileName`                         | `prompt.txt`                                       | `paths/paths.go:29`                   |
| v1 transcript            | `paths.TranscriptFileName`                     | `full.jsonl` (+ `.001`, `.002`, …)                 | `paths/paths.go:30`                   |
| v1 transcript hash       | `paths.ContentHashFileName`                    | `content_hash.txt` (format `sha256:<hex>`)         | `paths/paths.go:38`, `committed.go:784` |
| v2 compact transcript    | `paths.CompactTranscriptFileName`              | `transcript.jsonl` (on `/main`, not migrated)      | `paths/paths.go:32`                   |
| v2 compact hash          | `paths.CompactTranscriptHashFileName`          | `transcript_hash.txt` (on `/main`, not migrated)   | `paths/paths.go:33`                   |
| v2 raw transcript        | `paths.V2RawTranscriptFileName`                | `raw_transcript` (+ `.001`, …) on `/full/*`        | `paths/paths.go:34`                   |
| v2 raw hash              | `paths.V2RawTranscriptHashFileName`            | `raw_transcript_hash.txt` on `/full/*`             | `paths/paths.go:35`                   |
| Sharded path             | `id.Path()`                                    | `<id[:2]>/<id[2:]>` (12-char lowercase hex)        | `checkpoint/id/id.go`                 |
| Trailer key              | `trailers.CheckpointTrailerKey`                | `Entire-Checkpoint`                                | `trailers/trailers.go:41`             |
| Chunk filename suffix    | `agent.ChunkSuffix`                            | `.%03d`                                            | `agent/chunking.go:19`                |

## 8. Source map

- Tool entry: `cmd/migrate-v2-checkpoints/main.go`
- History walk: `cmd/migrate-v2-checkpoints/history.go` —
  `discoverCheckpointHistoryWithSkippedOrphans` (line 55),
  `addV2OrphanCheckpoints` (line 364), `listV2MainCheckpointIDs`
  (line 408), `writeCheckpointList` (line 484, includes the `(orphan)`
  label), `writeDiscoveryWarnings` (line 497, prints the scope-skip
  warning).
- v2 ref auto-fetch: `cmd/migrate-v2-checkpoints/v2_preflight.go` —
  `ensureLatestV2Refs` (line 24), `fetchV2MainRef` (line 75),
  `fetchV2FullRefs` (line 101).
- v1 ref auto-fetch: `cmd/migrate-v2-checkpoints/v1_preflight.go` —
  `ensureLatestV1Ref` (line 23), `remoteRefExists` (line 55),
  `fetchV1Ref` (line 70).
- Migration loop: `cmd/migrate-v2-checkpoints/migration.go` —
  `migrateDiscoveredCheckpoints` (line 53), `migrateCheckpoint`
  (line 96, disables commit signing before v1 writes),
  `writeOptionsFromV2Content` (line 217),
  `writeMigrationReport` (line 252), `candidateCommitLabel` (line 291,
  emits `(orphan)`).
- v2 session author lookup: `cmd/migrate-v2-checkpoints/v2_author.go` —
  `findV2SessionAuthor` (line 19), `commitChangedPath` (line 66),
  `v2SessionMetadataPath` (line 95).
- v1 write: `cmd/entire/cli/checkpoint/committed.go` — `WriteCommitted`
  (line 72), `WithCommitSigningDisabled` (line 57),
  `writeStandardCheckpointEntries` (line 324),
  `writeSessionToSubdirectory` (line 418), `writeTranscript` (line 741),
  `findSessionIndex` (line 631), `SignCommitBestEffort` (line 2001).
- v2 read: `cmd/entire/cli/checkpoint/v2_read.go` — `ReadCommitted`
  (line 26), `ReadSessionMetadataAndPrompts` (line 205),
  `ReadSessionContent` (line 274), `readTranscriptFromFullRefs`
  (line 342), `readTranscriptFromRef` (line 540),
  `isV2ArchivedFullRefSuffix` (line 523).
- Schemas: `cmd/entire/cli/checkpoint/checkpoint.go` — `CheckpointSummary`
  (line 545), `CommittedMetadata` (line 444), `SessionFilePaths`
  (line 520).
- Trailer parsing: `cmd/entire/cli/trailers/trailers.go`.
- Chunking: `cmd/entire/cli/agent/chunking.go`.
- Sanitization (Codex only): `cmd/entire/cli/agent/codex/`
  (`SanitizePortableTranscript`).
- ID + sharded path: `cmd/entire/cli/checkpoint/id/id.go`.

## 9. Notes for re-use on other repos

- `--repo PATH` works from anywhere; you do not need to `cd`. Bear in mind
  the tool walks `refs/remotes/*/*` too, so if the local repo has stale
  remote refs the candidate list may include IDs whose underlying commits
  are only reachable via those remotes. That's still correct — those
  commits really did reference the IDs.
- `plan`, `--dry-run`, and `--apply` auto-fetch checkpoint refs from the repo's
  checkpoint remote (`ensureLatestV1Ref`, `ensureLatestV2Refs`). If the
  remote resolves, you get an up-to-date local copy of
  `refs/heads/entire/checkpoints/v1`, `refs/entire/checkpoints/v2/main`, and
  every `refs/entire/checkpoints/v2/full/*`; the tool errors if that remote
  does not advertise v1 or v2 /main. If the remote cannot be resolved at all,
  the tool only proceeds when local v1 and v2 /main refs are already present.
  `--list` does **not** auto-fetch — if you want a candidate universe that
  reflects the remote, refresh manually first:
  ```sh
  git -C "$REPO" fetch origin \
      'refs/heads/entire/checkpoints/v1:refs/heads/entire/checkpoints/v1'
  git -C "$REPO" fetch origin \
      'refs/entire/checkpoints/v2/*:refs/entire/checkpoints/v2/*'
  ```
- Orphan augmentation is enabled by default. Pass `--since` or `--head`
  if you intentionally want to exclude v2-only IDs from migration; the
  tool will still print a single-line warning summarising how many were
  skipped.
- The tool is **idempotent** in `--apply` mode. Re-running after a
  successful apply should produce `checkpoints eligible for migration: 0`
  modulo any new v2 data that landed in the meantime.
- The tool only writes to the local repo. After `--apply`, push the
  updated v1 branch yourself when ready (and only after §5 passes).
