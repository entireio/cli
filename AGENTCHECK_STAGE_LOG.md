# AgentCheck Stage Log

## A2 - Entire Checkpoint to AgentCheckContext

Owner: Teammate A

Goal: Build the context foundation that adapts Entire checkpoint/session/Git/Graph evidence into a stable AgentCheck contract.

Implemented:
- Added `Context`, `Checkpoint`, `Session`, `Prompt`, `GitEvidence`, `GraphContext`, and provenance structs.
- Added `Builder.Build` to read checkpoint summaries and session content through existing Entire APIs.
- Added `BuildFromRepository` to open the real Entire checkpoint facade for a repository root.
- Preserved raw developer prompts and subsequent scoped prompts.
- Preserved task checkpoint markers when session metadata exposes them.
- Associated Git commits by scanning `Entire-Checkpoint` trailers and honoring checkpoint `CommitSHA` anchors when present.
- Collected changed files with `gitops.DiffTreeFileList` and patch evidence with `git show`.
- Added optional Graph provider interface, `GraphCLIProvider` for `entire graph checkpoint`, and graceful unavailable state.
- Added focused tests for the A2 required cases.

Verification:
- `entire graph search --repo . --profile full --query "AgentCheck checkpoint context adapter using Entire checkpoint session git trailer graph evidence APIs"` was run with the repaired PATH but returned no output within a bounded wait and was interrupted.
- `gofmt` completed for AgentCheck Go files.
- `go test ./cmd/entire/cli/agentcheck/...` passed.
- `go build ./cmd/entire/cli/agentcheck/...` passed.

Scope deviations:
- No evaluation, trust scoring, verification runner, HTML report, or final CLI was implemented.
- No checkpoint storage, Codex hooks, transcript collectors, or Entire internals were modified.
