# WIP: Abandoned Session Attribution Bug

**Date**: 2026-01-30
**Status**: In Progress

## The Goal

Create a test script that validates: when a user abandons Session 1's changes (via `git restore`) and then Session 2 makes different changes, only Session 2 should be attributed in the final commit.

## Test Scenario

1. Session 1 runs, adds `hash_password()` to `main.py`
2. User does `git restore main.py` (discards Session 1's changes)
3. Session 2 runs, adds `get_random_number()` to `main.py`
4. User commits
5. **Expected**: Only Session 2 in metadata
6. **Actual**: Both sessions in metadata

## What We Built

- **Script**: `scripts/test-attribution-e2e-abandoned-session.sh`
- Based on `scripts/test-attribution-e2e-second-session.sh`

## Fixes Applied

### 1. Shadow Branch Suffix Allocation (`manual_commit_suffix.go`)

**Problem**: When Session 2 started, `handleLegacySuffix()` always returned suffix 1, causing Session 2 to overwrite Session 1's shadow branch.

**Fix**: Added `findNextAvailableSuffix()` to check for existing suffixed branches:

```go
func findNextAvailableSuffix(repo *git.Repository, baseCommitShort string) int {
    for suffix := 1; suffix <= 100; suffix++ {
        branchName := checkpoint.ShadowBranchNameForCommitWithSuffix(baseCommitShort, suffix)
        if !shadowBranchExists(repo, branchName) {
            return suffix
        }
    }
    return 101
}
```

**Result**: Session 1 gets `entire/<hash>-1`, Session 2 gets `entire/<hash>-2`. ✅

### 2. Content Matching Check (`manual_commit_hooks.go`)

**Problem**: `filterSessionsWithNewContent()` only checked if files overlapped, not if the actual content matched. Both sessions touched `main.py`, so both passed the filter.

**Fix**: Added `sessionContentMatchesStaged()` to compare checkpoint blob hashes against staged blob hashes:

```go
func (s *ManualCommitStrategy) sessionContentMatchesStaged(repo *git.Repository, state *SessionState, stagedHashes map[string]plumbing.Hash) bool {
    // Get session's shadow branch tree
    // For each touched file, compare checkpoint hash vs staged hash
    // Return true only if at least one file matches
}
```

**Result**: Not working yet - both sessions still being condensed. ❌

## Current Observations

Shadow branches before commit show correct separation:

```
entire/05500ab-1 (Session 1):
  main.py contains hash_password()

entire/05500ab-2 (Session 2):
  main.py contains get_random_number()
```

But the final metadata shows:
```json
{
  "session_count": 2,
  "session_ids": ["session-1-id", "session-2-id"]
}
```

## Debug Logging Added

Added file-based debug logging since git hooks redirect stderr:

```go
debugFile, _ := os.OpenFile("/tmp/entire-debug.log", ...)
debugLog("filterSessionsWithNewContent called with %d sessions", len(sessions))
```

## Key Question

**Is `sessionContentMatchesStaged()` being called and returning the correct value?**

The function should:
1. Get Session 1's shadow branch tree (`entire/<hash>-1`)
2. Get `main.py` blob hash from that tree (should be hash of `hash_password` version)
3. Compare with staged `main.py` blob hash (should be hash of `get_random_number` version)
4. Return `false` because they don't match
5. Session 1 should be filtered out

## Files Modified

1. `cmd/entire/cli/strategy/manual_commit_hooks.go`
   - Added `getStagedFileHashes()`
   - Added `sessionContentMatchesStaged()`
   - Modified `filterSessionsWithNewContent()` to use content matching
   - Added debug logging (temporary)

2. `cmd/entire/cli/strategy/manual_commit_suffix.go`
   - Added `findNextAvailableSuffix()`
   - Modified `handleLegacySuffix()` to use it

3. `scripts/test-attribution-e2e-abandoned-session.sh`
   - New test script

## To Resume

1. Clean up and run:
   ```bash
   mise run fmt && mise run lint
   rm -f /tmp/entire-debug.log
   ./scripts/test-attribution-e2e-abandoned-session.sh --keep
   cat /tmp/entire-debug.log
   ```

2. Check the debug log to see:
   - Is `filterSessionsWithNewContent` being called?
   - What are the staged hashes?
   - What does `sessionContentMatchesStaged` return for each session?

3. If the function is being called but returning wrong values, check:
   - Is the shadow branch reference correct?
   - Is `tree.File(filePath)` finding the file?
   - Are the hashes being compared correctly?

## Test Command

```bash
./scripts/test-attribution-e2e-abandoned-session.sh --keep
```

Use `--keep` to preserve the test repo for inspection.
