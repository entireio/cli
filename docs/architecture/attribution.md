# Attribution

## Overview

Attribution tracks how much of a commit came from the agent vs the user. When a user commits code after working with an AI agent, the CLI calculates what percentage of the changes were agent-written vs human-written.

This is captured in the `Entire-Attribution` trailer on commits:

```
feat: Add user authentication

Entire-Checkpoint: a3b2c4d5e6f7
Entire-Attribution: 73% agent (146/200 lines)
```

## The Challenge

Attribution seems simple: diff the base commit against the final commit and count who wrote what. But there's a fundamental challenge: **the agent and user interleave their work**.

A typical session looks like:

1. User writes some code
2. Agent runs, adds more code (checkpoint 1)
3. User edits the agent's code and adds their own
4. Agent runs again (checkpoint 2)
5. User makes final tweaks
6. User commits

The checkpoint snapshots capture the worktree state at each moment, but that state includes *both* agent work and user edits made since the last checkpoint. We need to untangle these contributions.

## Architecture

### Data Collection

Attribution data is collected at two points:

**1. At prompt start** (`CalculatePromptAttribution`)

Before each agent run, we capture what the user changed since the last checkpoint:
- `UserLinesAdded` / `UserLinesRemoved` - aggregate counts
- `UserAddedPerFile` - per-file breakdown of user additions

This happens *before* the agent runs, so we can cleanly separate "user edits between prompts" from "agent work during prompt".

**2. At commit time** (`CalculateAttributionWithAccumulated`)

When the user commits, we calculate final attribution by:
1. Summing accumulated user edits from all `PromptAttribution` records
2. Adding post-checkpoint user edits (shadow → head diff)
3. Calculating agent work (base → shadow minus accumulated user edits)
4. Computing the final percentage

### Key Files

- `manual_commit_attribution.go` - Core attribution calculation logic
- `manual_commit_types.go` - `PromptAttribution` struct definition
- `manual_commit_hooks.go` - Hook that triggers attribution calculation on commit

## Line Ownership Tracking

When a user modifies code, we need to know: did they modify agent lines or their own lines?

**Example problem:**
1. Agent adds 10 lines
2. User adds 5 lines of their own
3. User removes 3 lines and adds 3 different lines (a modification)

If those 3 removed lines were the user's own code, agent attribution should be unaffected. If they were agent lines, agent attribution should decrease.

### Approaches Considered

#### Git Blame Against Checkpoints

Use git's blame to trace line ownership through checkpoint commits.

```go
// Lines introduced in shadow commits = agent lines
func getLineOwnership(repo *git.Repository, baseCommit, shadowCommit plumbing.Hash, filePath string) (agentLines, userLines []int) {
    // git blame --porcelain <shadow-commit> -- <file>
}
```

| Aspect | Assessment |
|--------|------------|
| Accuracy | High |
| Complexity | High - requires shelling to git CLI |
| Performance | Expensive at attribution time |

#### Line Hash Tracking

Track ownership via content hashes of individual lines.

```go
type LineOwnership struct {
    AgentLineHashes map[string]bool  // SHA256 of line content
}
```

| Aspect | Assessment |
|--------|------------|
| Accuracy | Low |
| Complexity | Medium |
| Performance | Fast lookups |

**Fatal flaw:** Common lines like `}`, `return nil`, and blank lines have identical hashes. When a user removes a `}`, we can't determine which one (agent's or user's) was removed.

#### Position-Aware Diff Tracking

Track line ownership by file position, updating ranges as edits happen.

```go
type FileOwnership struct {
    AgentRanges []LineRange  // e.g., [{Start: 5, End: 15}]
}
```

| Aspect | Assessment |
|--------|------------|
| Accuracy | High |
| Complexity | Very High - complex range arithmetic |
| Performance | Must process every diff sequentially |

#### Per-File Pool Heuristic (Selected)

Track aggregate ownership counts per file. Use LIFO assumption: users modify their own recent additions before touching agent code.

```go
// PromptAttribution includes per-file tracking
type PromptAttribution struct {
    UserLinesAdded   int
    UserLinesRemoved int
    UserAddedPerFile map[string]int  // Per-file breakdown
}

// When user removes lines, assume they remove their own first
func estimateUserSelfModifications(
    accumulatedUserAddedPerFile map[string]int,
    postCheckpointUserRemovedPerFile map[string]int,
) int {
    var selfModified int
    for filePath, removed := range postCheckpointUserRemovedPerFile {
        userAddedToFile := accumulatedUserAddedPerFile[filePath]
        selfModified += min(removed, userAddedToFile)
    }
    return selfModified
}
```

| Aspect | Assessment |
|--------|------------|
| Accuracy | Medium - good for common case |
| Complexity | Low |
| Performance | Fast |

### Why Per-File Pools

1. **Solves the common case**: When users modify their own recent code (the typical pattern), attribution remains accurate

2. **Sidesteps line identity problems**: We don't need to solve "which `}` was removed" - we reason at the file level

3. **Reasonable assumption**: "Users modify their own recent code before agent code" matches typical editing patterns

4. **Low implementation cost**: Just adds a map to `PromptAttribution`

### Trade-offs Accepted

- **Imprecise for adversarial cases**: If a user deliberately removes agent code while keeping their own, attribution will be slightly off
- **File-level granularity**: We can't distinguish modifications within different functions of the same file

These are acceptable because:
- Attribution is informational, not security-critical
- Users have no incentive to game attribution
- Perfect accuracy would require reimplementing git-blame

## Calculation Flow

```
Session Start (base commit)
    │
    ▼
┌─────────────────────────────────┐
│ User edits files                │
└─────────────────────────────────┘
    │
    ▼
┌─────────────────────────────────┐
│ Prompt submitted                │
│ → CalculatePromptAttribution()  │
│ → Capture user edits per file   │
└─────────────────────────────────┘
    │
    ▼
┌─────────────────────────────────┐
│ Agent runs, creates checkpoint  │
│ → Shadow branch updated         │
└─────────────────────────────────┘
    │
    ▼
    ... (repeat for multiple prompts) ...
    │
    ▼
┌─────────────────────────────────┐
│ User makes final edits          │
└─────────────────────────────────┘
    │
    ▼
┌─────────────────────────────────┐
│ User commits                    │
│ → CalculateAttributionWith-     │
│   Accumulated()                 │
│ → Sum all PromptAttributions    │
│ → Add post-checkpoint edits     │
│ → Estimate self-modifications   │
│ → Calculate final percentage    │
└─────────────────────────────────┘
    │
    ▼
Commit with Entire-Attribution trailer
```

## Example Calculation

**Scenario:**
- Agent adds 10 lines to `main.go`
- User adds 5 lines to `main.go` (between checkpoints)
- User modifies 3 of their own lines (removes 3, adds 3 different)

**Calculation:**
```
base → shadow: 15 lines added (10 agent + 5 user in snapshot)
accumulatedUserAdded: 5 (from PromptAttribution)
totalAgentAdded: 15 - 5 = 10

shadow → head: +3 added, -3 removed (user's modification)
postCheckpointUserAdded: 3
postCheckpointUserRemoved: 3

totalUserAdded: 5 + 3 = 8
totalUserRemoved: 3
totalHumanModified: min(8, 3) = 3

// Per-file tracking kicks in:
userSelfModified: min(3 removed from main.go, 5 user added to main.go) = 3
humanModifiedAgent: 3 - 3 = 0  // No agent lines were modified!

agentLinesInCommit: 10 - 0 = 10  // Agent attribution preserved
totalCommitted: 10 + 5 = 15
agentPercentage: 10/15 = 66.7%
```

Without per-file tracking, we would have incorrectly subtracted 3 from agent lines, giving 46.7% instead of 66.7%.

## References

- Implementation: `cmd/entire/cli/strategy/manual_commit_attribution.go`
- Types: `cmd/entire/cli/strategy/manual_commit_types.go`
- Tests: `cmd/entire/cli/strategy/manual_commit_attribution_test.go`
