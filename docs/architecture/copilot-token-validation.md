# Copilot Token Validation

This document defines the manual validation pass for Copilot CLI token accounting.

Use it when changing:

- `cmd/entire/cli/agent/copilotcli/transcript.go`
- `cmd/entire/cli/strategy/manual_commit_condensation.go`
- Any code that affects `CheckpointTranscriptStart`, checkpoint metadata, or session token backfill

## Why Copilot Needs Its Own Validation

Copilot CLI exposes two different kinds of token data in `events.jsonl`:

- `assistant.message.data.outputTokens`: scoped to a single assistant response
- `session.shutdown.data.modelMetrics[*].usage`: aggregate for the entire session

That means Entire must treat the same transcript in two different ways:

- Checkpoint metadata must stay scoped to `CheckpointTranscriptStart`
- Session state for `entire status` should use the full-session aggregate once `session.shutdown` exists

If this logic regresses, earlier checkpoints can suddenly show the same token count as the whole session.

## Source of Truth

For Copilot token validation, check the raw stored data first:

- Checkpoint metadata: `entire/checkpoints/v1:<checkpoint-shard>/0/metadata.json`
- Session state: `.git/entire-sessions/<session-id>.json`

`entire explain` and `entire status` are useful views, but the raw metadata is the durable source of truth.

## Preferred Manual Path: Mock Session Harness

Use the mock-session harness to create a reproducible two-turn Copilot session and inspect the raw metadata directly:

```bash
scripts/test-copilot-token-metadata.sh --keep-temp
```

The script:

- Creates a disposable repo
- Writes mock Copilot `events.jsonl` transcripts
- Runs the real `entire hooks copilot-cli ...` commands
- Creates two commits with real Entire trailers and condensation
- Prints both checkpoint `metadata.json` payloads plus the final session state file
- Fails if checkpoint metadata or session state tokens regress

Expected output from the harness:

- Checkpoint 1 metadata shows only turn 1 scoped usage
- Checkpoint 2 metadata shows only turn 2 scoped usage
- Session state shows the full `session.shutdown` aggregate

## Prerequisites

- Copilot CLI is installed and authenticated
- Entire is enabled for Copilot CLI in the test repo
- `jq` is available for transcript inspection
- You are working in a disposable git repo

## Transcript Location

Copilot CLI stores session transcripts at:

```text
~/.copilot/session-state/<session-id>/events.jsonl
```

To find the latest session transcript:

```bash
SESSION_DIR="$(ls -td ~/.copilot/session-state/* | head -n1)"
TRANSCRIPT="$SESSION_DIR/events.jsonl"
printf '%s\n' "$TRANSCRIPT"
```

## Live Session Scenario

Validate a two-turn Copilot session where both turns produce checkpoints in the same session.

### 1. Create a Disposable Repo

```bash
tmpdir="$(mktemp -d)"
cd "$tmpdir"
git init
git config user.name "Entire Test"
git config user.email "entire@example.com"
printf 'start\n' > README.md
git add README.md
git commit -m "init"
entire enable --agent copilot-cli
```

### 2. Run Turn 1 and Commit

In Copilot CLI, run a small prompt that changes one file, then commit it.

Example:

```text
Create alpha.txt with the text "alpha", then stop.
```

Commit the change:

```bash
git add alpha.txt
git commit -m "Add alpha"
```

Capture checkpoint 1:

```bash
CP1="$(git log -1 --format=%B | sed -n 's/^Entire-Checkpoint: //p' | tail -n1)"
entire explain --checkpoint "$CP1" --no-pager
```

Record the `tokens` row value for checkpoint 1 as `CP1_TOKENS_INITIAL`.

### 3. Run Turn 2 in the Same Copilot Session and Commit

In the same Copilot session, run another prompt that changes a different file.

Example:

```text
Create beta.txt with the text "beta", then stop.
```

Commit the second change:

```bash
git add beta.txt
git commit -m "Add beta"
```

Capture checkpoint 2:

```bash
CP2="$(git log -1 --format=%B | sed -n 's/^Entire-Checkpoint: //p' | tail -n1)"
entire explain --checkpoint "$CP2" --no-pager
```

### 4. End the Copilot Session

Exit the Copilot session cleanly so `session.shutdown` is written to `events.jsonl`.

Then verify the latest transcript path:

```bash
SESSION_DIR="$(ls -td ~/.copilot/session-state/* | head -n1)"
TRANSCRIPT="$SESSION_DIR/events.jsonl"
test -f "$TRANSCRIPT"
```

### 5. Compute the Expected Full-Session Total from `session.shutdown`

```bash
jq -s '
  map(select(.type == "session.shutdown")) | last |
  .data.modelMetrics
  | map(.usage.inputTokens + .usage.outputTokens + .usage.cacheReadTokens + .usage.cacheWriteTokens)
  | add
' "$TRANSCRIPT"
```

Record this as `SESSION_TOTAL`.

### 6. Verify Entire Session Status Uses the Full-Session Total

Run:

```bash
entire status
```

Expected:

- The active or most recent Copilot session shows a token total that matches `SESSION_TOTAL`, allowing only display rounding differences
- This confirms condensation backfilled session state from the full transcript

### 7. Verify Checkpoint 1 Did Not Change to the Full-Session Total

Re-run:

```bash
entire explain --checkpoint "$CP1" --no-pager
```

Expected:

- Checkpoint 1 still shows the same `tokens` row value as `CP1_TOKENS_INITIAL`
- Checkpoint 1 does not jump to `SESSION_TOTAL`
- Checkpoint 1 will usually be lower than `SESSION_TOTAL`

This is the key regression check.

### 8. Verify Checkpoint 2 Is Also Scoped

Run:

```bash
entire explain --checkpoint "$CP2" --no-pager
```

Expected:

- Checkpoint 2 token count reflects only the second checkpoint's scoped transcript portion
- Checkpoint 2 should not equal `SESSION_TOTAL` unless the second checkpoint genuinely covers the entire session

## Expected Outcomes

The fix is behaving correctly when all of these are true:

- `entire status` reflects the final Copilot `session.shutdown` aggregate
- `entire explain --checkpoint "$CP1"` stays stable before and after turn 2 / session shutdown
- Earlier checkpoints do not all collapse to the same final session-wide token total

## Failure Signatures

The old bug has likely regressed if you see either of these:

- Checkpoint 1 token count changes after a later turn finishes
- Checkpoint 1 token count becomes identical to the final `session.shutdown` total

## Notes

- Copilot checkpoint totals are intentionally different from Claude checkpoint totals.
- Claude stores per-message `input/output/cache` usage on assistant messages, so Claude can compute exact checkpoint-scoped totals.
- Copilot only exposes authoritative `input/cache/output` usage as a session-wide aggregate at `session.shutdown`, so checkpoint-scoped Copilot counts are necessarily more limited.
