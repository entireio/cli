# Replay Lab

Replay Lab turns historical Entire checkpoints into private agent benchmarks.
It answers: "Which agent/model actually works best on this repository's real
tasks?"

## Command Surface

```bash
entire replay checkpoint <checkpoint-id> --agent codex --test-cmd "go test ./..." --timeout 20m
entire replay checkpoint <checkpoint-id> --agent claude-code --keep-worktree
entire replay checkpoint <checkpoint-id> --agent gemini --json
entire replay report <run-id>

entire eval run --from-checkpoints --limit 5 --agent claude-code,codex --test-cmd "go test ./..."
entire eval run --checkpoint <checkpoint-id> --checkpoint <checkpoint-id> --agent codex
entire eval report <eval-id>
```

Supported launchable replay agents:

- `claude-code`
- `codex`
- `gemini`

## How One Replay Works

1. Resolve the checkpoint id to the real checkpoint commit.
2. Read the checkpoint metadata and recover the original user prompt. Prompt
   sources are tried in order: stored prompts, review prompt metadata,
   transcript prompts, then summary intent.
3. Create a temporary git worktree at the checkpoint parent commit.
4. Launch the selected agent with the recovered prompt.
5. Commit the replay result in the temporary worktree so the diff is stable.
6. Compare replay output to the real checkpoint commit.
7. Optionally run `--test-cmd` inside the replay worktree.
8. Save a JSON report under the repository git common directory.
9. Remove the replay worktree unless `--keep-worktree` is set.

Replay intentionally starts from the checkpoint parent. The target checkpoint
commit is the answer key, and the replay worktree is the candidate answer.

## Pass Criteria

A replay is `passed` when the agent process succeeds and the optional test
command succeeds. If no `--test-cmd` is provided, process success is enough for
pass/fail, while the file and risk metrics still describe quality.

A replay is `failed` when the agent command exits non-zero, times out, cannot be
launched, or the optional test command fails. Failed runs still save captured
output, diffs, metrics, and warnings when available.

An eval ranks agents across all selected checkpoint tasks. Rankings prioritize:

1. Pass rate
2. File recall against the original checkpoint commit
3. File precision
4. Optional semantic similarity
5. Lower risk count
6. Lower duration
7. Lower token usage when reported

## Metrics

- `file_recall`: percentage of original changed files also changed by the
  replay.
- `file_precision`: percentage of replay changed files that were part of the
  original change.
- `missing_files`: original changed files not touched by the replay.
- `extra_files`: files touched only by the replay.
- `risk_count`: heuristic count of missing risky files, extra risky files, and
  missing tests for source changes.
- `semantic_similarity`: optional score from `entire-sem` when the executable is
  available on `PATH`.
- `input_tokens`, `output_tokens`, `total_tokens`: token usage when the agent
  reports it.

Risk heuristics intentionally favor actionable warnings over perfect static
analysis. They flag security, auth, credential, payment, database, migration,
deployment, config, workflow, environment, and infrastructure paths, plus source
changes that do not include test changes.

## Storage

Reports are written under the git common directory, outside the working tree:

```text
.git/entire-replay/runs/<run-id>.json
.git/entire-replay/evals/<eval-id>.json
```

This keeps benchmark data local to the repository without adding tracked files.
Use `entire replay report <run-id>` and `entire eval report <eval-id>` to render
saved reports. Add `--json` to either command for automation.

## Isolation

Replay worktrees run with:

- `ENTIRE_REPLAY=1`
- git hook execution disabled via `core.hooksPath=/dev/null`
- inherited git environment variables stripped before launching the agent

This prevents replay runs from creating normal Entire hook side effects or
leaking the caller's git directory into the isolated worktree.

## Failure Handling

Replay Lab saves as much evidence as possible:

- agent output is capped in saved reports to avoid huge JSON files
- diffs are capped and marked as truncated when necessary
- timeout errors preserve any diff the agent produced before cancellation
- evals skip unavailable agents instead of failing the whole benchmark
- checkpoint resolution/build failures become failed eval rows for visibility

## Key Files

- `cmd/entire/cli/replay.go` - command definitions, replay execution, metrics,
  report storage, rendering
- `cmd/entire/cli/replay_test.go` - replay/eval behavior, ranking, risk,
  persistence, timeout, and help coverage
- `cmd/entire/cli/labs.go` - labs registry entries for `replay` and `eval`
