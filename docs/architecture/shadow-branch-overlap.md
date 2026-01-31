# Shadow Branch: Single Branch with Overlap Detection

This document describes the simplified shadow branch architecture that prevents data corruption when sessions interleave.

## Problem

The current shadow branch architecture stores both file content and metadata on a single branch per base commit (`entire/<hash>`). This causes corruption when sessions interleave:

### 1. Ghost Files

When a user dismisses changes (`git restore .`) and starts new work, the old session's files remain in the shadow tree. The new checkpoint only processes files present in the worktree, leaving dismissed files as orphaned entries.

**How it happens:**

```go
// In buildTreeWithChanges (temporary.go)
// Start with OLD checkpoint's tree
entries := make(map[string]object.TreeEntry)
FlattenTree(s.repo, baseTree, "", entries)  // entries now has old session's files

// Only process files from current worktree
for _, file := range modifiedFiles {
    if !fileExists(absPath) {
        delete(entries, file)  // only deletes if file is IN modifiedFiles
        continue
    }
    // add/update file
}
// Files from old tree NOT in modifiedFiles stay as ghost entries
```

### 2. Content Corruption

New checkpoints overwrite file entries from previous sessions. If session A modified line 30 and session B (after dismiss) modifies line 5 of the same file, the shadow tree now contains only line 5's change. The git history shows "removed line 30, added line 5" even though session B never touched line 30.

**Example:**

```
Session A checkpoint tree:
  file.ts: line 30 changed to "bar"

User runs: git restore .

Session B checkpoint (reads file.ts from disk):
  file.ts: line 5 changed to "baz", line 30 is original "foo"

Shadow branch diff (A → B):
  - line 30: bar    # "removed" but Session B never touched this!
  + line 5: baz     # added
```

### 3. Attribution Corruption

Attribution calculations compare the shadow tree against base and HEAD. Mixed/stale data from multiple sessions produces incorrect agent contribution metrics.

## Solution: Single Branch with Overlap Detection

Use a single shadow branch per base commit (`entire/<hash>`), but detect when work streams diverge using a simple file overlap check.

### Core Logic

The overlap check happens in two phases to prevent incorrectly continuing on stale shadow branches:

**Phase 1: At prompt start (before any file modifications)**

Check if current worktree overlaps with shadow branch files and set a flag:

1. **Worktree is clean** → Set `ShouldResetShadowBranch = true`
2. **Files modified AND overlap with shadow branch** → Set `ShouldResetShadowBranch = false`
3. **Files modified BUT no overlap** → Set `ShouldResetShadowBranch = true`

**Phase 2: At checkpoint time (if SaveChanges is called)**

1. If `ShouldResetShadowBranch = true` → Reset shadow branch, then checkpoint
2. If `ShouldResetShadowBranch = false` → Continue on existing shadow branch
3. Clear the flag after use (reset only once per prompt)

**Phase 3: At prompt end (if no checkpoint occurred)**

Clear the `ShouldResetShadowBranch` flag and check again on next prompt.

### Why Check at Prompt Start?

Checking at prompt start (before modifications) prevents this bug:

```
Session A: modifies file1.ts, checkpointed
User: git stash (worktree clean)
Session B prompt: "implement login in file1.ts"
  Claude: modifies file1.ts
```

If we checked overlap at checkpoint time, we'd see:
- Shadow branch has: file1.ts (from Session A)
- Worktree has: file1.ts (from Session B)
- Overlap detected → Continue ❌ WRONG!

By checking at prompt start (before Claude modifies anything), we see:
- Shadow branch has: file1.ts
- Worktree has: nothing (stashed)
- No overlap → Set shouldReset = true ✅ CORRECT!

### Decision Matrix

| Worktree state at prompt start | Shadow touched files | Flag set | Checkpoint behavior |
|--------------------------------|---------------------|----------|---------------------|
| Clean | - | shouldReset = true | Reset on first checkpoint |
| Modified | No overlap | shouldReset = true | Reset on first checkpoint |
| Modified | Overlap exists | shouldReset = false | Continue on shadow branch |

## Implementation

### Session State Changes

Add a flag to track whether the shadow branch should be reset on next checkpoint:

```go
// In strategy/manual_commit_types.go

type SessionState struct {
    SessionID                string            `json:"session_id"`
    BaseCommit               string            `json:"base_commit"`
    CheckpointCount          int               `json:"checkpoint_count"`
    FilesTouched             []string          `json:"files_touched,omitempty"`
    ShouldResetShadowBranch  bool              `json:"should_reset_shadow_branch"`  // NEW
    // ... existing fields
}
```

### Phase 1: Prompt Start Hook

Check overlap before the prompt makes any file modifications:

```go
// In strategy/manual_commit_hooks.go (or new file)

// OnPromptStart is called at the start of each user prompt, before any file modifications.
// It checks if the current worktree overlaps with the shadow branch and sets a flag
// to determine whether the shadow branch should be reset on the next checkpoint.
func (s *ManualCommitStrategy) OnPromptStart(ctx context.Context, sessionID string) error {
    state, err := s.stateStore.LoadSessionState(sessionID)
    if err != nil {
        return fmt.Errorf("failed to load session state: %w", err)
    }

    repo, err := s.getRepo()
    if err != nil {
        return err
    }

    shadowBranch := checkpoint.ShadowBranchNameForCommit(state.BaseCommit)

    // Check if shadow branch exists
    _, err = repo.Reference(plumbing.NewBranchReferenceName(shadowBranch), true)
    if err != nil {
        if err == plumbing.ErrReferenceNotFound {
            // No shadow branch yet, nothing to check
            state.ShouldResetShadowBranch = false
            return s.stateStore.SaveSessionState(state)
        }
        return err
    }

    // Get current worktree status (BEFORE prompt makes changes)
    modifiedFiles, err := s.getModifiedFiles(repo)
    if err != nil {
        return err
    }

    // Clean worktree → reset on first checkpoint
    if len(modifiedFiles) == 0 {
        logging.Debug(ctx, "clean worktree at prompt start - will reset shadow branch on checkpoint")
        state.ShouldResetShadowBranch = true
        return s.stateStore.SaveSessionState(state)
    }

    // Check file overlap
    shadowTouched, err := s.getFilesTouchedByShadow(repo, shadowBranch)
    if err != nil {
        // On error reading shadow, reset to be safe
        logging.Warn(ctx, "failed to read shadow branch files",
            slog.String("error", err.Error()),
        )
        state.ShouldResetShadowBranch = true
        return s.stateStore.SaveSessionState(state)
    }

    overlap := fileSetIntersection(shadowTouched, modifiedFiles)
    if len(overlap) == 0 {
        logging.Debug(ctx, "no file overlap at prompt start - will reset shadow branch on checkpoint",
            slog.Int("shadow_files", len(shadowTouched)),
            slog.Int("worktree_files", len(modifiedFiles)),
        )
        state.ShouldResetShadowBranch = true
    } else {
        logging.Debug(ctx, "file overlap detected at prompt start - will continue on shadow branch",
            slog.Int("overlap_count", len(overlap)),
        )
        state.ShouldResetShadowBranch = false
    }

    return s.stateStore.SaveSessionState(state)
}

// OnPromptEnd is called at the end of each user prompt if no checkpoint was created.
// It clears the reset flag so we check again on the next prompt.
func (s *ManualCommitStrategy) OnPromptEnd(ctx context.Context, sessionID string) error {
    state, err := s.stateStore.LoadSessionState(sessionID)
    if err != nil {
        return fmt.Errorf("failed to load session state: %w", err)
    }

    // Clear flag - we'll check again on next prompt
    state.ShouldResetShadowBranch = false

    return s.stateStore.SaveSessionState(state)
}

### Phase 2: Checkpoint Time (SaveChanges)

Use the flag set at prompt start to decide whether to reset:

```go
// In strategy/manual_commit.go

func (s *ManualCommitStrategy) SaveChanges(ctx SaveContext) (*SaveResult, error) {
    // ... existing setup ...

    shadowBranch := checkpoint.ShadowBranchNameForCommit(state.BaseCommit)

    // Get modified files from worktree
    modifiedFiles, err := s.getModifiedFiles(repo)
    if err != nil {
        return nil, err
    }

    // Use flag set at prompt start to decide whether to reset
    if state.ShouldResetShadowBranch {
        logging.Info(logCtx, "resetting shadow branch (no overlap at prompt start)",
            slog.String("shadow_branch", shadowBranch),
        )
        if err := s.resetShadowBranch(repo, shadowBranch); err != nil {
            return nil, fmt.Errorf("failed to reset shadow branch: %w", err)
        }
        // Reset checkpoint count since we're starting fresh
        state.CheckpointCount = 0
        state.FilesTouched = nil

        // Clear flag after use (only reset once)
        state.ShouldResetShadowBranch = false
    } else {
        logging.Debug(logCtx, "continuing on shadow branch",
            slog.String("shadow_branch", shadowBranch),
        )
    }

    // Write checkpoint
    result, err := store.WriteTemporary(ctx.Context, checkpoint.WriteTemporaryOptions{
        BaseCommit:    state.BaseCommit,
        SessionID:     state.SessionID,
        Checkpoint:    ctx.Checkpoint,
        ModifiedFiles: modifiedFiles,
        // ... rest of options
    })
    if err != nil {
        return nil, err
    }

    // Update session state
    state.CheckpointCount++
    state.FilesTouched = mergeStringSlices(state.FilesTouched, modifiedFiles)

    if err := s.stateStore.SaveSessionState(state); err != nil {
        return nil, fmt.Errorf("failed to save session state: %w", err)
    }

    // ... return result
}
```

### Helper Functions

```go
// In strategy/manual_commit.go

func (s *ManualCommitStrategy) getFilesTouchedByShadow(
    repo *git.Repository,
    shadowBranch string,
) ([]string, error) {
    shadowRef, err := repo.Reference(plumbing.NewBranchReferenceName(shadowBranch), true)
    if err != nil {
        return nil, err
    }

    shadowCommit, err := repo.CommitObject(shadowRef.Hash())
    if err != nil {
        return nil, err
    }

    shadowTree, err := shadowCommit.Tree()
    if err != nil {
        return nil, err
    }

    // Get base commit for comparison
    if len(shadowCommit.ParentHashes) == 0 {
        return nil, fmt.Errorf("shadow commit has no parent")
    }
    baseHash := shadowCommit.ParentHashes[0]
    baseCommit, err := repo.CommitObject(baseHash)
    if err != nil {
        return nil, err
    }
    baseTree, err := baseCommit.Tree()
    if err != nil {
        return nil, err
    }

    // Find all files that differ between base and shadow
    changes, err := baseTree.Diff(shadowTree)
    if err != nil {
        return nil, err
    }

    files := make([]string, 0, len(changes))
    for _, change := range changes {
        // Get file path (from either source or destination)
        if change.To.Name != "" {
            files = append(files, change.To.Name)
        } else if change.From.Name != "" {
            files = append(files, change.From.Name)
        }
    }

    return files, nil
}

func fileSetIntersection(a, b []string) []string {
    setA := make(map[string]bool, len(a))
    for _, f := range a {
        setA[f] = true
    }

    var result []string
    for _, f := range b {
        if setA[f] {
            result = append(result, f)
        }
    }

    return result
}

func (s *ManualCommitStrategy) resetShadowBranch(
    repo *git.Repository,
    shadowBranch string,
) error {
    // Delete the existing shadow branch
    branchRef := plumbing.NewBranchReferenceName(shadowBranch)
    if err := repo.Storer.RemoveReference(branchRef); err != nil {
        if err != plumbing.ErrReferenceNotFound {
            return fmt.Errorf("failed to delete shadow branch: %w", err)
        }
        // Branch doesn't exist, nothing to reset
    }

    return nil
}
```

### Build Full Trees (Not Cumulative)

The key to avoiding ghost files is building each checkpoint tree from scratch based on the current worktree state:

```go
// In checkpoint/temporary.go - modify buildTreeWithChanges

func (s *GitStore) buildTreeWithChanges(
    baseTree *object.Tree,
    modifiedFiles []string,
    worktreePath string,
) (*object.Tree, error) {
    // Start with EMPTY tree entries
    entries := make(map[string]object.TreeEntry)

    // ONLY copy directories from base tree (not files)
    // This ensures directory structure but no ghost files
    if err := copyDirectoryStructure(s.repo, baseTree, "", entries); err != nil {
        return nil, err
    }

    // Add/update ONLY files that exist in current worktree
    for _, file := range modifiedFiles {
        absPath := filepath.Join(worktreePath, file)

        // Read file from disk
        content, err := os.ReadFile(absPath)
        if err != nil {
            if os.IsNotExist(err) {
                // File was deleted, skip it
                delete(entries, file)
                continue
            }
            return nil, err
        }

        // Create blob and add to entries
        hash, err := s.repo.Storer.SetEncodedObject(
            &plumbing.MemoryObject{...},
        )
        if err != nil {
            return nil, err
        }

        entries[file] = object.TreeEntry{
            Name: filepath.Base(file),
            Mode: filemode.Regular,
            Hash: hash,
        }
    }

    // Build tree from entries
    return buildTreeFromEntries(s.repo, entries)
}
```

## Scenarios Covered

### 1. Continue Work (Common Case)
```
Session A: modifies file1.ts → checkpoint on entire/abc123
Session B prompt starts:
  - Worktree: file1.ts (from Session A)
  - Shadow: file1.ts
  - Overlap detected → shouldReset = false
Session B: modifies file1.ts more → SaveChanges → continue on entire/abc123 ✅
```

### 2. Dismiss and Start Fresh
```
Session A: modifies file1.ts → checkpoint on entire/abc123
User: git restore .
Session B prompt starts:
  - Worktree: clean
  - shouldReset = true
Session B: modifies file2.ts → SaveChanges → reset entire/abc123 → checkpoint ✅
```

### 3. Partial Dismiss
```
Session A: modifies file1.ts, file2.ts → checkpoint
User: git restore file1.ts (keeps file2.ts)
Session B prompt starts:
  - Worktree: file2.ts
  - Shadow: file1.ts, file2.ts
  - Overlap (file2) → shouldReset = false
Session B: modifies file2.ts, file3.ts → SaveChanges → continue ✅
Session B's checkpoint reads full worktree (file2 + file3)
```

### 4. Stash, Answer Questions, Unstash (Safe)
```
Session A: modifies file1.ts → checkpoint
User: git stash
Session B prompt starts:
  - Worktree: clean
  - shouldReset = true
Session B: just answers questions, no code changes → no SaveChanges
  - OnPromptEnd clears shouldReset flag
User: git stash apply
Session C prompt starts:
  - Worktree: file1.ts (from stash)
  - Shadow: file1.ts
  - Overlap detected → shouldReset = false
User commits → condensation uses Session A's data ✅
```

### 5. Stash, New Work on Same Files (Correctly Reset)
```
Session A: modifies file1.ts → checkpoint
User: git stash (worktree clean)
Session B prompt: "implement login in file1.ts"
  - Prompt starts: worktree clean → shouldReset = true ✅
  - Claude: modifies file1.ts
  - SaveChanges: reset shadow branch → checkpoint with Session B's work ✅
  - Shadow now has Session B's version of file1.ts, not Session A's
```

**Why this works:** By checking at prompt start (before modifications), we see the worktree is clean, so we know Session A's work was dismissed. If we checked at checkpoint time, we'd see file1.ts in both places and incorrectly think it's a continuation.

### 6. Stash, New Work on Different Files (Limitation)
```
Session A: modifies file1.ts → checkpoint
User: git stash
Session B prompt starts:
  - Worktree: clean
  - shouldReset = true
Session B: modifies file2.ts → SaveChanges → reset shadow branch ⚠️
Session A's checkpoint lost
User: git stash apply → file1.ts returns
User commits both files → condensation only has Session B's transcript ❌

This is an accepted limitation for v1.
```

## Trade-offs

### Pros
- **Much simpler code** - no suffix tracking, no suffix migration, no complex decision logic
- **Handles common cases well** - continue work, dismiss work, partial dismissals
- **No ghost files** - only current worktree files are in checkpoint trees
- **No content corruption** - reset ensures clean slate when work diverges

### Cons
- **Stash + new work on different files** - loses old session transcript (scenario 6)
  - This is acceptable because:
    - The suffix approach doesn't solve this either (requires content matching across all suffixes)
    - It's a rare scenario (most stash workflows are temporary)
    - User can work around it by committing before stashing
    - We can add smarter detection later if needed
- **Requires hook integration** - relies on prompt start/end hooks to set flag correctly

### Comparison to Suffix Approach

| Aspect | Single Branch | Suffixes |
|--------|--------------|----------|
| Code complexity | Low | High |
| Ghost files | Fixed ✅ | Fixed ✅ |
| Content corruption | Fixed ✅ | Fixed ✅ |
| Continue work | Works ✅ | Works ✅ |
| Dismiss work | Works ✅ | Works ✅ |
| Stash + new work | Lost transcript ❌ | Lost transcript ❌* |
| Branch cleanup | Simple (1 branch) | Complex (N suffixes) |

*The suffix approach also fails this scenario unless we implement content matching across all suffixes, which is explicitly deferred as "too complex" in the original design.

## Multi-Session Warning

### Current Behavior

When a second session starts while there's an existing session with uncommitted checkpoints, both sessions can proceed and their checkpoints will interleave on the shadow branch. This can be confusing for users who may not realize there's already an active session.

### Opt-In Warning (Inverted Setting)

Add an opt-in warning that alerts users when starting a new session while another session has uncommitted work:

**Setting:** `enable-multisession-warning` (default: `false`)

When enabled:

1. **At session start**, check if there's an existing session with:
   - Same base commit
   - Uncommitted checkpoints (shadow branch exists, not yet condensed)

2. **If found**, show warning:
   ```
   Warning: An existing session has uncommitted work on this branch.

   Session ID: 2026-01-31-abc123
   Checkpoints: 3 uncommitted
   Files touched: src/auth.go, src/server.go

   Starting a new session will merge with the existing session.
   - If files overlap: Continue on same shadow branch
   - If no overlap: Reset shadow branch (old session data will be lost)

   Options:
   1. Cancel and commit existing work first
   2. Continue (merge sessions)
   ```

3. **If user chooses to continue**, the normal overlap detection proceeds:
   - Prompt start checks overlap
   - Sets `ShouldResetShadowBranch` flag
   - On checkpoint, follows normal reset/continue logic

### Implementation

```go
// In strategy/manual_commit_hooks.go

type MultiSessionWarningConfig struct {
    Enabled bool `json:"enabled"`
}

func (s *ManualCommitStrategy) CheckMultiSessionWarning(
    ctx context.Context,
    sessionID string,
) error {
    // Check if warning is enabled
    config := s.getMultiSessionWarningConfig()
    if !config.Enabled {
        return nil
    }

    state, err := s.stateStore.LoadSessionState(sessionID)
    if err != nil {
        return err
    }

    // Find other sessions with same base commit
    allSessions, err := s.stateStore.ListSessionStates()
    if err != nil {
        return err
    }

    var conflictingSessions []SessionState
    for _, otherState := range allSessions {
        if otherState.SessionID == sessionID {
            continue
        }
        if otherState.BaseCommit == state.BaseCommit && otherState.CheckpointCount > 0 {
            conflictingSessions = append(conflictingSessions, otherState)
        }
    }

    if len(conflictingSessions) == 0 {
        return nil
    }

    // Show warning and prompt user
    return s.showMultiSessionWarning(ctx, conflictingSessions)
}

func (s *ManualCommitStrategy) showMultiSessionWarning(
    ctx context.Context,
    conflictingSessions []SessionState,
) error {
    // Build warning message
    var msg strings.Builder
    msg.WriteString("Warning: Existing session(s) with uncommitted work:\n\n")

    for _, session := range conflictingSessions {
        msg.WriteString(fmt.Sprintf("Session ID: %s\n", session.SessionID))
        msg.WriteString(fmt.Sprintf("Checkpoints: %d uncommitted\n", session.CheckpointCount))
        if len(session.FilesTouched) > 0 {
            msg.WriteString(fmt.Sprintf("Files touched: %s\n", strings.Join(session.FilesTouched, ", ")))
        }
        msg.WriteString("\n")
    }

    msg.WriteString("Starting a new session will merge with the existing session.\n")
    msg.WriteString("- If files overlap: Continue on same shadow branch\n")
    msg.WriteString("- If no overlap: Reset shadow branch (old session data will be lost)\n\n")

    // Prompt user
    var shouldContinue bool
    prompt := huh.NewConfirm().
        Title(msg.String()).
        Description("Continue with new session?").
        Affirmative("Continue (merge sessions)").
        Negative("Cancel").
        Value(&shouldContinue)

    if err := prompt.Run(); err != nil {
        return err
    }

    if !shouldContinue {
        return fmt.Errorf("session cancelled by user")
    }

    return nil
}
```

### Migration from Old Setting

The old `disable-multisession-warning` setting is inverted:

| Old Setting | New Setting | Behavior |
|-------------|-------------|----------|
| Not set (default) | `enable-multisession-warning: false` | No warning (default) |
| `disable-multisession-warning: true` | `enable-multisession-warning: false` | No warning |
| `disable-multisession-warning: false` | `enable-multisession-warning: true` | Show warning |

Migration logic:
```go
func migrateMultiSessionWarningSetting(config *Config) {
    if config.Has("disable-multisession-warning") {
        oldValue := config.Get("disable-multisession-warning").(bool)
        config.Set("enable-multisession-warning", !oldValue)
        config.Delete("disable-multisession-warning")
    }
}
```

### When to Check

The multi-session warning check should happen:

1. **At session initialization** - when a new session is created or resumed
2. **Before OnPromptStart** - so user can cancel before any overlap checking

Flow:
```
Session start
  ↓
Check multi-session warning (if enabled)
  ↓ (user confirms)
OnPromptStart (check overlap, set flag)
  ↓
User prompt
  ↓
SaveChanges (use flag to reset or continue)
```

### Benefits

- **Explicit opt-in** - users who want safety warnings can enable it
- **Clear consequences** - warning explains what will happen (merge or reset)
- **Escape hatch** - users can cancel and commit existing work first
- **No change to default behavior** - warning is off by default, doesn't interrupt existing workflows

## Future Enhancements

If scenario 6 (stash + new work on different files) becomes problematic, we can add:

1. **Multi-suffix support** - fall back to suffixes when reset would lose data
2. **Content matching** - check worktree against all shadow branches to find best match
3. **Stash detection** - warn user if we detect stashed work might be lost

For now, the simple approach gives us 95% of the benefit with 20% of the complexity.

## Migration

No migration needed - this is a simplification of the existing single-branch approach. The current `entire/<hash>` branches work as-is. We're just adding smarter reset logic to prevent corruption.
