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
  excluding `entire/checkpoints/v1` and `entire/trails/v1`).
- For each commit on those tips, parses `Entire-Checkpoint: <id>` trailers
  (`trailers.ParseAllCheckpoints`, key constant
  `trailers/trailers.go:41`). One commit can carry many trailers (squash
  merges).
- Produces a list of `discoveredCheckpoint{ID, Commits}` — every checkpoint ID
  ever referenced in commit history, plus the commits that mention it.
- `--since <commit>`/positional commit narrows to commits not reachable from
  the named commit. `--head <commit>` restricts to a single tip.
- Discovery is **not** v2-specific. It is a universe of "every checkpoint we
  ever ran on a commit reachable from a real ref."

### 1.2 Migration filter (`cmd/migrate-v2-checkpoints/migration.go`)

For each discovered checkpoint:

1. Read v1 summary from `entire/checkpoints/v1`. If present, collect existing
   v1 session IDs by reading each session's `metadata.json` (`session_id`
   field).
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
     write to v1 via `GitStore.WriteCommitted` using v2-sourced fields.

A checkpoint is **eligible** if at least one v2 session is missing from v1 and
fully readable from v2. The candidate's `sessions=N` is that net count, not
the v2 session count.

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

`<N>` is the v1 slot. New sessions append (`findSessionIndex` in
`committed.go:326`); if v1 already had session 0 and v2 contributes one new
session, it lands in v1 slot 1. v1 indices and v2 indices for the **same**
checkpoint can differ; only `session_id` is invariant across the two stores.

Chunking note: `full.jsonl` is chunked via `agent.ChunkTranscript`. Chunks are
`full.jsonl`, `full.jsonl.001`, `full.jsonl.002`, … (`agent/chunking.go:122`
with `ChunkSuffix = ".%03d"`). Index 0 has no suffix.

Codex caveat: for sessions whose agent is `codex`, `writeTranscript` applies
`codex.SanitizePortableTranscript` before chunking and hashing
(`committed.go:745-747`). The bytes written to v1 may differ from the bytes
read out of v2's `/full/*`, but they are still self-consistent against the new
v1 `content_hash.txt`.

## 2. Run modes & expected report shape

```text
$ migrate-v2-checkpoints [--repo PATH] [--since SHA | SHA] [--head SHA] \
                         (--list | --dry-run | --apply)
```

Default mode is `plan` (same output as `--dry-run`).

`--list` produces one line per checkpoint:
```text
<checkpoint-id> <commit-short-sha> [<commit-short-sha> ...]
```
This is the **universe** discovered in history — NOT the eligible set.

`--dry-run` / `--apply` produces:
```text
Migration plan:                          (or "Migration result:" on --apply)
  discovered checkpoints: D
  already present v1 sessions: A
  missing v2 checkpoint metadata: M1
  missing required v2 session metadata: M2
  missing raw transcripts: M3
  checkpoints eligible for migration: EC
  sessions eligible for migration: ES
  migrated checkpoints: ...              (--apply only)
  migrated sessions: ...                 (--apply only)
  checkpoints to migrate:
    <id> sessions=N commits=<sha>[,<sha>...]
```

Invariants that should always hold on the report:

- `EC ≤ D`.
- `ES ≥ EC` (each eligible checkpoint contributes ≥ 1 eligible session).
- `ES = Σ candidate.SessionCount`. The candidate list is exhaustive.
- On `--apply`: `migrated checkpoints = EC` and `migrated sessions = ES` if
  no write errors. Anything less means a partial write failure — re-run the
  tool and the remainder should re-appear as eligible.
- `D = EC + (checkpoints with all v2 sessions in v1) + (checkpoints with
  any missing-metadata / missing-transcript failure modes)`.
- Counter sums for skipped sessions:
  `A + M2 + M3 = (Σ over all v2 sessions in checkpoints whose v2 summary
  exists) − ES`. Useful for spot-checking after `--apply`: if `A` is large
  and `EC` is small, most v2 checkpoints are already mirrored.

## 3. Validation procedure

The procedure below is the same regardless of repo. Substitute `$REPO` and
`$TOOL` per environment:

```sh
REPO=/path/to/some-repo                 # e.g. ~/entire/marvin
TOOL=~/entire/cli/.worktrees/review/migrate-v2-checkpoints
cd "$REPO"
```

### 3.1 Pre-flight: confirm both stores exist

```sh
git -C "$REPO" show-ref entire/checkpoints/v1
git -C "$REPO" show-ref refs/entire/checkpoints/v2/main
git -C "$REPO" show-ref refs/entire/checkpoints/v2/full/current
git -C "$REPO" for-each-ref 'refs/entire/checkpoints/v2/full/*' \
    --format='%(refname)'
```

If `entire/checkpoints/v1` is missing the migration can still apply (it will
be created), but if the v2 refs are missing there is nothing to migrate.

Also sanity-check the head of v2 isn't surprising — a recent commit means v2
was being dual-written; a long-stale v2 head matches the rollback narrative:

```sh
git -C "$REPO" log -1 --format='%h %ci %s' refs/entire/checkpoints/v2/main
```

### 3.2 Step A — sanity check the dry-run report

```sh
"$TOOL" --repo "$REPO" --dry-run | tee /tmp/migrate.plan
```

Spot-check the counter math against §2:

```sh
grep -E "^  (discovered|already|missing|checkpoints eligible|sessions eligible)" \
    /tmp/migrate.plan
```

- `EC ≤ D` and `ES ≥ EC`.
- For each candidate line, parse `sessions=N` and sum — must equal `ES`.

```sh
awk '/^    [0-9a-f]{12} sessions=/ {sub(/sessions=/,"",$2); s+=$2} END {print s}' \
    /tmp/migrate.plan
# Should equal the "sessions eligible for migration" value.
```

### 3.3 Step B — confirm every candidate is genuinely v2-only-or-partial

For every candidate `<id>`:

```sh
ID=02d9783342a2     # example
SHARD=${ID:0:2}/${ID:2}

# Does v2 /main carry this checkpoint?
git -C "$REPO" cat-file -p \
    refs/entire/checkpoints/v2/main:"$SHARD/metadata.json" \
    | jq '{checkpoint_id, sessions: [.sessions[].metadata]}'

# Does v1 already carry it? (Either the path doesn't exist, or the session
# IDs differ.)
git -C "$REPO" cat-file -p \
    entire/checkpoints/v1:"$SHARD/metadata.json" 2>/dev/null \
    | jq '{checkpoint_id, sessions: [.sessions[].metadata]}' \
    || echo "(absent in v1)"
```

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

# IDs already in v1 (any session present).
git -C "$REPO" ls-tree -r entire/checkpoints/v1 2>/dev/null \
    | awk '$4 ~ /^[0-9a-f]{2}\/[0-9a-f]{10}\/metadata\.json$/ { \
               split($4, p, "/"); print p[1] p[2] \
           }' \
    | sort -u > /tmp/v1_ids.txt
comm -23 /tmp/v2_ids.txt /tmp/v1_ids.txt > /tmp/v2_only_ids.txt
wc -l /tmp/v2_only_ids.txt
```

Every ID in `v2_only_ids.txt` should be either a candidate, or — if v2 has
no session metadata for it / no raw transcript — a contributor to the
`missing v2 checkpoint metadata` / `missing raw transcripts` counters.

A quick predicate: the eligible candidate count plus the missing-metadata
and missing-raw counters should equal or exceed the v2-only set. If it's
less, something is being silently dropped.

```sh
EC=$(grep "checkpoints eligible" /tmp/migrate.plan | awk '{print $NF}')
M1=$(grep "missing v2 checkpoint metadata" /tmp/migrate.plan | awk '{print $NF}')
M3=$(grep "missing raw transcripts" /tmp/migrate.plan | awk '{print $NF}')
echo "v2-only on disk: $(wc -l < /tmp/v2_only_ids.txt)"
echo "EC=$EC  M1=$M1  M3=$M3   (EC + M1 + M3 must be >= v2-only count)"
```

(`>=` rather than `=` because `M1`/`M3` are counted per-checkpoint over the
entire discovered universe, not only the v2-only set.)

### 3.4 Step C — confirm commit-list accuracy

The report's `commits=...` are short SHAs of commits in history whose message
carries `Entire-Checkpoint: <id>`. Verify directly:

```sh
ID=02d9783342a2
git -C "$REPO" log --all --format='%h %s' --grep "Entire-Checkpoint: $ID"
```

The set of short SHAs that this prints should match the report's
`commits=…` for that ID. If they differ:

- Extra in the report but absent here: the discovery walk picked up a tip
  this `--all` view doesn't include (rare).
- Extra here but absent in the report: a tip was filtered out
  (`entire/checkpoints/v1`, `entire/trails/v1`, or `HEAD` aliases — the
  filter is in `history.go:182-205`).

A commit may also appear under multiple candidate IDs if it's a squash
merge with multiple trailers; that's expected.

### 3.5 Step D — DRY-RUN INSPECTION of session count

For each candidate, the report claims `sessions=N`. Confirm:

```sh
ID=02d9783342a2
SHARD=${ID:0:2}/${ID:2}

# Sessions advertised by the v2 summary (from /main).
git -C "$REPO" cat-file -p \
    refs/entire/checkpoints/v2/main:"$SHARD/metadata.json" \
    | jq -r '.sessions | length'

# Session IDs in v2 (read each session's own metadata.json — that field is
# what the migration tool dedupes against, not summary order).
V2_SESSION_COUNT=$(git -C "$REPO" cat-file -p \
    refs/entire/checkpoints/v2/main:"$SHARD/metadata.json" \
    | jq -r '.sessions | length')
for i in $(seq 0 $((V2_SESSION_COUNT-1))); do
    git -C "$REPO" cat-file -p \
        refs/entire/checkpoints/v2/main:"$SHARD/$i/metadata.json" \
        | jq -r '.session_id'
done | sort -u > /tmp/v2_sids.txt

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

# Expected eligible: v2 minus v1, by session ID.
comm -23 /tmp/v2_sids.txt /tmp/v1_sids.txt | wc -l
# This number must equal the report's "sessions=N" for this checkpoint.
```

Repeat for a random sample (5–10) across the candidate list. If your
sample matches 1:1, the report's accounting is trustworthy.

## 4. Apply the migration

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
- The local repo has the v2 refs. If `git -C "$REPO" show-ref
  refs/entire/checkpoints/v2/main` is empty, the migration will silently
  count everything as "missing v2 checkpoint metadata" and write nothing.
  Pre-fetch:

  ```sh
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

# Apply. Tee the report into /tmp/migrate-${REPO_NAME}.applied — §5 reads it back.
"$TOOL" --repo "$REPO" --apply | tee "$APPLIED_REPORT"

# Sanity-check the report.
grep -E "^  (checkpoints eligible|sessions eligible|migrated)" "$APPLIED_REPORT"
#   migrated checkpoints == checkpoints eligible
#   migrated sessions    == sessions eligible
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
- **Per-checkpoint atomicity, not transactional.** Each candidate is
  written as its own commit on v1. If `--apply` errors out partway
  through, earlier candidates remain written and later ones are
  un-written; the next run will pick up the rest.
- **Roll back** by resetting v1 back to `$PRE_APPLY_TIP`:

  ```sh
  # Only if you need to undo — this discards the new commits locally.
  git -C "$REPO" update-ref refs/heads/entire/checkpoints/v1 "$PRE_APPLY_TIP"
  ```

  Safe before any push. Destructive after push.

### Operator checkpoint

**Stop here. Run the apply command yourself and confirm:**

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

### 5.1 Step E — root `metadata.json` (CheckpointSummary) on v1

For each candidate, decode the v1 root metadata and confirm:

```sh
ID=02d9783342a2
SHARD=${ID:0:2}/${ID:2}

git -C "$REPO" cat-file -p "entire/checkpoints/v1:$SHARD/metadata.json" | jq .
```

Expected shape (schema lives at
`cmd/entire/cli/checkpoint/checkpoint.go:527-562`):

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

Field-by-field check against the v2 summary on `/main` for the same ID:

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
- `combined_attribution`/`token_usage` may differ if the v1 store
  aggregates across all sessions present and v1 already had different
  sessions. For purely v2-only checkpoints (the typical case the user
  cares about) these should match the v2 summary exactly, since the
  migration uses `summary.CombinedAttribution` from v2 verbatim
  (`migration.go:199`) and per-session token usage is replayed from v2.

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
for n in $(seq 0 $((V1_LEN-1))); do
    SID=$(git -C "$REPO" cat-file -p \
            "entire/checkpoints/v1:$SHARD/$n/metadata.json" \
          | jq -r '.session_id')
    if [ "$SID" = "$WANT_SID" ]; then
        V1_SLOT=$n; break
    fi
done
echo "session $WANT_SID lives in v1 slot $V1_SLOT"
```

Then diff the per-session metadata, comparing **fields that are expected to
survive migration** (`migration.go:173-205` lists them explicitly):

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

- `created_at` is replayed from v2's `created_at` and also used as v1's
  `CommitTime` (`migration.go:178-179`). The two timestamps in the v1 file
  should be identical when serialised.
- The migration sets `HasReview = session.Kind(meta.Kind).IsReview()`
  (`migration.go:204`). For non-review kinds this is `false` and may have
  been absent (omitempty) in v2; that's still a match.
- `cli_version` on the v1 session may differ from v2's. The migration
  doesn't pass `CLIVersion`, so v1 inherits whatever default the writer
  applies — generally an empty value or the current binary's version. Not
  a correctness issue.
- v1 writes the new `combined_attribution` and aggregated `token_usage`
  onto the **root** `metadata.json` from the migrating session's data. If
  there were prior v1 sessions, the root summary on v1 already aggregated
  them; only the new session's session-level metadata matters for §4.2.

Schema sanity per session:

```sh
git -C "$REPO" cat-file -p "entire/checkpoints/v1:$SHARD/$V1_SLOT/metadata.json" \
  | jq -e 'has("checkpoint_id") and has("session_id") and has("created_at")' \
  > /dev/null && echo OK
```

### 5.3 Step G — `prompt.txt` content

The migration joins v2 prompts (split form on disk) back into a single
`prompt.txt` via `SplitPromptContent` round-trip. The bytes should match
the v2 content:

```sh
git -C "$REPO" cat-file -p \
    "refs/entire/checkpoints/v2/main:$SHARD/$V2_SLOT/prompt.txt" \
    | sha256sum
git -C "$REPO" cat-file -p \
    "entire/checkpoints/v1:$SHARD/$V1_SLOT/prompt.txt" \
    | sha256sum
```

Both digests should match. If they don't, inspect with a `diff -u` between
the two `cat-file -p` outputs to see whether it's an ordering / separator
issue.

### 5.4 Step H — raw transcript & `content_hash.txt`

This is the most important check. Two layers:

1. **Self-consistency on v1**: the value in `content_hash.txt` must equal
   `sha256:<hex>` of the reassembled `full.jsonl[.NNN]` content.
2. **Cross-store match (non-Codex agents)**: reassembled v1 bytes should
   equal reassembled v2 `raw_transcript[.NNN]` bytes, and v1's
   `content_hash.txt` should equal v2's `raw_transcript_hash.txt`.

Reassemble logic: ordered list `full.jsonl`, `full.jsonl.001`,
`full.jsonl.002`, … For most agents this is JSONL with `\n` separators
between chunks (`agent/chunking.go:108-118`); for `vogon`, OpenCode etc.
the agent's own `ReassembleTranscript` is used at read time. For
validation, byte-concatenation in chunk order is what the v1 writer
hashed (`committed.go:784` — the hash is over `transcriptBytes` BEFORE
chunking), so the easier check is to read the original v1 input bytes
back via the v1 store API, OR to validate that each chunk blob is what
the v1 writer would have produced.

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
COMPUTED="sha256:$(sha256sum "$tmp" | awk '{print $1}')"
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
(`committed.go:745-747`). The v1 self-consistency check above is still
required in that case.

### 5.5 Step I — bulk sweep

Once the per-checkpoint procedure is established, sweep every migrated
checkpoint:

```sh
TOOL=~/entire/cli/.worktrees/review/migrate-v2-checkpoints
"$TOOL" --repo "$REPO" --dry-run \
  | awk '/^    [0-9a-f]{12} sessions=/ {print $1}' > /tmp/candidates.txt
wc -l /tmp/candidates.txt
```

Then for each ID in `/tmp/candidates.txt`, run:

- §4.1 root metadata diff (`grep -q` for errors).
- §4.2 per-session field diff for every session ID that the candidate
  brought in.
- §4.4 hash check on every transcript chunk set.

A single shell loop is fine, and the validation completes in seconds per
checkpoint. Surface any non-empty diffs or any `MISMATCH` lines.

### 5.6 After validation passes

You're done with this runbook only after every step in §5 produced the
expected result on every candidate. Publishing the migration is **out of
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
| Candidate `commits=` is empty                                  | Shouldn't happen by construction (discovery groups by commit). Investigate the bug.                                | File a bug.                                                                             |
| `sessions=N` for a candidate doesn't match the §3.5 expected   | Either v1 already has the session (so report should have lower N), or session IDs are non-unique within v2.        | Inspect; non-unique session IDs are a v2 corruption.                                    |
| Post-apply, `content_hash.txt` ≠ recomputed SHA-256            | Codex agent + ours-vs-original sanitization difference, OR a bug. Confirm `agent` field on the session.            | If non-Codex, file a bug with chunk listing + bytes.                                    |
| Post-apply, `content_hash.txt` matches but v2's `raw_transcript_hash.txt` doesn't | Codex sanitization (expected) OR transcript was rewritten in transit. Confirm agent first.               | If non-Codex, file a bug.                                                               |
| Re-running `--dry-run` after `--apply` still lists the same candidates | Apply failed silently or didn't get pushed before re-fetch. Look at the `migrated sessions` count.         | Re-run with verbose logging; check that v1 branch actually advanced.                    |

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
- History walk: `cmd/migrate-v2-checkpoints/history.go`
- Migration loop: `cmd/migrate-v2-checkpoints/migration.go`
- v1 write: `cmd/entire/cli/checkpoint/committed.go` — `WriteCommitted`
  (line 52), `writeStandardCheckpointEntries` (line 310),
  `writeSessionToSubdirectory` (line 404), `writeTranscript` (line 720),
  `findSessionIndex` (line 326).
- v2 read: `cmd/entire/cli/checkpoint/v2_read.go` — `ReadCommitted`
  (line 24), `ReadSessionMetadataAndPrompts` (line 205),
  `ReadSessionContent` (line 274), `readTranscriptFromFullRefs`
  (line 339), `readTranscriptFromRef` (line 540).
- Schemas: `cmd/entire/cli/checkpoint/checkpoint.go` — `CheckpointSummary`
  (line 527), `CommittedMetadata` (line 443), `SessionFilePaths`
  (line 517).
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
- If the v2 refs aren't fetched locally (the default refspec excludes
  `refs/entire/*`), discovery will still find IDs from trailers but the
  per-checkpoint v2 reads will fail with "missing v2 checkpoint metadata."
  Pre-fetch with:
  ```sh
  git -C "$REPO" fetch origin \
      'refs/entire/checkpoints/v2/*:refs/entire/checkpoints/v2/*'
  ```
- The tool is **idempotent** in `--apply` mode. Re-running after a
  successful apply should produce `checkpoints eligible for migration: 0`
  modulo any new v2 data that landed in the meantime.
- The tool only writes to the local repo. After `--apply`, push the
  updated v1 branch yourself when ready.
