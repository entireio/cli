# AgentCheck Architecture

## A2 Boundary

AgentCheck uses Entire as the source of truth for checkpoint and session context.

The current boundary is:

`Entire checkpoint/session readers -> agentcheck.Builder -> agentcheck.Context -> future evaluators`

Future evaluators should depend on `cmd/entire/cli/agentcheck.Context`, not on checkpoint storage paths, refs, branches, or Codex hook internals.

Production callers can use `agentcheck.BuildFromRepository` to open the existing Entire checkpoint facade for a repository root. Tests and future CLI wiring can still inject a `Reader` directly into `Builder` when they need a narrower surface.

## Context Sources

- Checkpoint metadata comes from `checkpoint.CheckpointReader`.
- Session metadata, prompt text, and transcript bytes come from `checkpoint.SessionReader`.
- Prompt splitting uses `checkpoint.SplitPromptContent`.
- Commit association uses `trailers.ParseAllCheckpoints` against commit messages, plus checkpoint `CommitSHA` anchors when present.
- Changed file evidence uses `gitops.DiffTreeFileList`.
- Diff evidence uses `git show` against associated commits.
- Graph evidence is optional and injected through `GraphProvider`; `GraphCLIProvider` uses `entire graph checkpoint <id> --json` when configured by a caller.

## Unavailable Evidence

Missing optional evidence is represented explicitly with availability flags and reasons. The builder does not fabricate prompts, transcripts, commits, diffs, task records, or Graph results.

## Out Of Scope

A2 does not include evaluation rules, trust verdicts, verification runners, reports, or command presentation.
