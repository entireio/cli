# AgentCheck Codebase Snapshot

## Package

`cmd/entire/cli/agentcheck`

## Files

- `context.go`: AgentCheck-owned context contract and evidence/provenance structs.
- `context_builder.go`: builder that adapts existing Entire checkpoint/session readers and checkpoint-associated Git evidence into `Context`, plus a production repository constructor and optional Graph CLI provider.
- `context_builder_test.go`: focused tests for valid checkpoints, session preservation, prompt preservation, changed files, checkpoint-associated Git evidence, optional Graph, missing checkpoints, and unavailable data.

## Context Contract

`Context` is the boundary for downstream evaluators. Evaluators should consume this package and should not read Entire checkpoint storage directly.

Guaranteed when `Build` succeeds:
- `CheckpointID`
- checkpoint-level metadata available from `checkpoint.CheckpointSummary`
- `Sessions` entries for every session listed by the checkpoint summary
- raw prompt text when stored by Entire
- `FilesTouched` from checkpoint summary
- provenance sources for checkpoint, session, Git, and Graph evidence

Optional or unavailable:
- `DeveloperPrompt` and `ScopedPrompts` are empty when Entire stored no prompt text.
- `Transcript.Available` is false when session transcript bytes are empty or unavailable.
- `Git.AssociatedCommits`, `Git.ChangedFiles`, and `Git.Diff` are empty with unavailable reasons when no repo, repo root, or matching commit evidence is available.
- `Graph.Available` is false when no Graph provider is configured or the provider returns an error. `GraphCLIProvider` can call `entire graph checkpoint <id> --json` when a caller supplies it.
- `TaskRecords` contains task tool-use IDs only when session metadata exposes task markers; deeper durable task records remain unavailable until a stable reader surface is introduced.

## Entire APIs Reused

- `checkpoint.CheckpointReader`
- `checkpoint.SessionReader`
- `checkpoint.Open`
- `checkpoint.ReadCheckpoint`
- `checkpoint.SplitPromptContent`
- `checkpoint.CheckpointSummary`
- `checkpoint.Metadata`
- `checkpoint.SessionContent`
- `trailers.ParseAllCheckpoints`
- `gitops.DiffTreeFileList`
- `gitrepo.OpenPath`
- `settings.WithWorktreeRoot`
- `strategy.CheckpointReadRemotes`

## Stage Boundary

This package does not implement evaluators, scoring, verification execution, reporting, or CLI presentation.
