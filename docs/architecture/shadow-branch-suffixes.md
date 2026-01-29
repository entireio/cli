# Shadow Branch Isolation via Suffixes

This document describes a proposed improvement to the shadow branch architecture to prevent data corruption when sessions interleave.

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

```go
// In CalculateAttributionWithAccumulated
attribution = CalculateAttributionWithAccumulated(
    baseTree,      // before any session
    shadowTree,    // has ghost files and corrupted content!
    headTree,      // what user is committing
    ...
)
```

### 4. No Dismiss Detection

Git provides no hooks for `restore`, `reset`, or `checkout`, so we cannot detect when work is discarded.

## Why Not Just Reset the Shadow Branch?

When a new session starts and the worktree doesn't match the shadow branch, it might seem safe to reset or delete the old shadow data. However, this would break legitimate workflows involving `git stash`:

**Simple scenario (handled correctly):**

1. Session A does work, creates checkpoints on `-1`
2. User runs `git stash` (worktree is now clean)
3. Session B starts—perhaps just to ask a question, no code changes
4. User runs `git stash apply` (Session A's work returns to worktree)
5. User commits

At step 5, we need Session A's checkpoint data for condensation. If we had reset the shadow branch at step 3 (seeing a clean worktree), that data would be lost.

With the suffix approach, Session B would only create `-2` if it actually makes code changes that trigger a checkpoint. If Session B just answers questions without modifying files, no checkpoint is created, `-1` remains untouched, and condensation works correctly.

**The harder scenario:**

1. Session A creates checkpoints on `-1`
2. User runs `git stash`
3. Session B makes code changes → creates `-2`
4. User dismisses Session B's changes (`git restore .`)
5. User runs `git stash apply` (Session A's work returns)
6. User commits

Now we have both `-1` and `-2`, and need to condense from `-1`. Detecting that we should "go back" to `-1` (rather than creating `-3`) would require recognizing that the worktree now matches `-1`'s content—essentially running the continuation heuristic against all existing suffixes, not just the highest one. This adds complexity and may not be worth implementing initially. The data on `-1` is at least preserved and could be condensed if we match it correctly at commit time.

## Solution: Suffixed Shadow Branches

Shadow branches use numeric suffixes to isolate independent streams of work:

```
entire/af5a770-1  ← First stream of work
entire/af5a770-2  ← Second stream (after first was dismissed)
entire/af5a770-3  ← Third stream (after second was dismissed)
```

When a session creates a checkpoint, it either **continues on an existing suffix** (if previous work is still present in the worktree) or **creates a new suffix** (if previous work was dismissed). This decision runs every time a checkpoint is needed—not just when a session starts.

This handles scenarios like:

1. User works with Claude, creates checkpoints on `-1`
2. Closes Claude (session state persists)
3. Runs `git restore .` to dismiss changes
4. Resumes with a new prompt
5. On checkpoint, we detect the dismiss → session now continues on `-2`

The session ID can stay the same, but the suffix changes because the previous work stream was abandoned. Each suffix maintains its own isolated file tree, preventing ghost files and content corruption.

## Implementation

### Session State Changes

```go
// In session/state.go or strategy/manual_commit_types.go

type SessionState struct {
    SessionID                string            `json:"session_id"`
    BaseCommit               string            `json:"base_commit"`
    ShadowBranchSuffix       int               `json:"shadow_branch_suffix"`  // NEW
    CheckpointCount          int               `json:"checkpoint_count"`
    FilesTouched             []string          `json:"files_touched,omitempty"`
    // ... existing fields
}
```

### Shadow Branch Naming

```go
// In checkpoint/temporary.go

const (
    ShadowBranchPrefix     = "entire/"
    ShadowBranchHashLength = 7
)

// ShadowBranchNameForCommitWithSuffix returns the suffixed shadow branch name.
func ShadowBranchNameForCommitWithSuffix(baseCommit string, suffix int) string {
    hash := baseCommit
    if len(hash) > ShadowBranchHashLength {
        hash = hash[:ShadowBranchHashLength]
    }
    return fmt.Sprintf("%s%s-%d", ShadowBranchPrefix, hash, suffix)
}

// FindHighestSuffix finds the highest existing suffix for a base commit.
func (s *GitStore) FindHighestSuffix(baseCommit string) (int, error) {
    hash := baseCommit
    if len(hash) > ShadowBranchHashLength {
        hash = hash[:ShadowBranchHashLength]
    }
    prefix := ShadowBranchPrefix + hash + "-"

    highest := 0
    iter, err := s.repo.Branches()
    if err != nil {
        return 0, err
    }

    iter.ForEach(func(ref *plumbing.Reference) error {
        name := ref.Name().Short()
        if strings.HasPrefix(name, prefix) {
            suffixStr := strings.TrimPrefix(name, prefix)
            if n, err := strconv.Atoi(suffixStr); err == nil && n > highest {
                highest = n
            }
        }
        return nil
    })

    return highest, nil
}
```

### Suffix Decision Heuristic

```go
// In strategy/manual_commit_suffix.go (new file)

// SuffixDecision represents the result of the suffix heuristic.
type SuffixDecision struct {
    Suffix      int
    IsNew       bool
    Reason      string
}

// DecideSuffix determines which suffix to use for checkpointing.
// Returns the suffix number and whether it's a new suffix.
func (s *ManualCommitStrategy) DecideSuffix(
    repo *git.Repository,
    baseCommit string,
    currentSuffix int,
) (SuffixDecision, error) {
    store, err := s.getCheckpointStore()
    if err != nil {
        return SuffixDecision{}, err
    }

    // Find highest existing suffix
    highest, err := store.FindHighestSuffix(baseCommit)
    if err != nil {
        return SuffixDecision{}, err
    }

    // No existing suffix - start with 1
    if highest == 0 {
        return SuffixDecision{Suffix: 1, IsNew: true, Reason: "no existing suffix"}, nil
    }

    // Get worktree status
    worktree, err := repo.Worktree()
    if err != nil {
        return SuffixDecision{}, err
    }

    status, err := worktree.Status()
    if err != nil {
        return SuffixDecision{}, err
    }

    // Step 1: Clean worktree = new suffix
    worktreeModified := getModifiedFilesFromStatus(status)
    if len(worktreeModified) == 0 {
        return SuffixDecision{
            Suffix: highest + 1,
            IsNew:  true,
            Reason: "worktree is clean",
        }, nil
    }

    // Get files touched by highest suffix
    shadowTouched, err := s.getFilesTouchedBySuffix(repo, baseCommit, highest)
    if err != nil {
        // On error, create new suffix to be safe
        return SuffixDecision{
            Suffix: highest + 1,
            IsNew:  true,
            Reason: "failed to read shadow branch",
        }, nil
    }

    // Step 2: No overlap = new suffix
    overlap := intersect(shadowTouched, worktreeModified)
    if len(overlap) == 0 {
        return SuffixDecision{
            Suffix: highest + 1,
            IsNew:  true,
            Reason: "no file overlap with shadow branch",
        }, nil
    }

    // Step 3: Check if agent's changes are preserved
    preserved, err := s.agentChangesPreserved(repo, baseCommit, highest, overlap)
    if err != nil {
        // On error, create new suffix to be safe
        return SuffixDecision{
            Suffix: highest + 1,
            IsNew:  true,
            Reason: "failed to compare content",
        }, nil
    }

    if preserved {
        return SuffixDecision{
            Suffix: highest,
            IsNew:  false,
            Reason: "agent's changes still present",
        }, nil
    }

    return SuffixDecision{
        Suffix: highest + 1,
        IsNew:  true,
        Reason: "agent's changes not found in worktree",
    }, nil
}

// agentChangesPreserved checks if any lines added by the agent are still in the worktree.
func (s *ManualCommitStrategy) agentChangesPreserved(
    repo *git.Repository,
    baseCommit string,
    suffix int,
    filesToCheck []string,
) (bool, error) {
    // Get base tree
    baseHash := plumbing.NewHash(baseCommit)
    baseCommitObj, err := repo.CommitObject(baseHash)
    if err != nil {
        return false, err
    }
    baseTree, err := baseCommitObj.Tree()
    if err != nil {
        return false, err
    }

    // Get shadow tree
    shadowBranch := ShadowBranchNameForCommitWithSuffix(baseCommit, suffix)
    shadowRef, err := repo.Reference(plumbing.NewBranchReferenceName(shadowBranch), true)
    if err != nil {
        return false, err
    }
    shadowCommit, err := repo.CommitObject(shadowRef.Hash())
    if err != nil {
        return false, err
    }
    shadowTree, err := shadowCommit.Tree()
    if err != nil {
        return false, err
    }

    // Get worktree root
    wt, err := repo.Worktree()
    if err != nil {
        return false, err
    }
    worktreeRoot := wt.Filesystem.Root()

    for _, file := range filesToCheck {
        baseContent := getFileFromTree(baseTree, file)
        shadowContent := getFileFromTree(shadowTree, file)
        worktreeContent := readFileFromDisk(filepath.Join(worktreeRoot, file))

        // Skip if shadow didn't change this file from base
        if baseContent == shadowContent {
            continue
        }

        // Check if any agent-added lines are in worktree
        if hasAgentLinesPreserved(baseContent, shadowContent, worktreeContent) {
            return true, nil
        }
    }

    return false, nil
}

// hasAgentLinesPreserved checks if any lines added by the agent exist in the worktree.
func hasAgentLinesPreserved(baseContent, shadowContent, worktreeContent string) bool {
    baseLines := toLineSet(baseContent)
    shadowLines := strings.Split(shadowContent, "\n")
    worktreeLines := toLineSet(worktreeContent)

    for _, line := range shadowLines {
        // Skip empty/whitespace lines
        trimmed := strings.TrimSpace(line)
        if trimmed == "" {
            continue
        }

        // Line was added by agent (not in base) and still in worktree
        if !baseLines[line] && worktreeLines[line] {
            return true
        }
    }

    return false
}

// For fuzzy matching, normalize whitespace:
func hasAgentLinesPreservedFuzzy(baseContent, shadowContent, worktreeContent string) bool {
    baseLines := toNormalizedLineSet(baseContent)
    shadowLines := strings.Split(shadowContent, "\n")
    worktreeLines := toNormalizedLineSet(worktreeContent)

    for _, line := range shadowLines {
        normalized := strings.TrimSpace(line)
        if normalized == "" {
            continue
        }

        if !baseLines[normalized] && worktreeLines[normalized] {
            return true
        }
    }

    return false
}

func toLineSet(content string) map[string]bool {
    set := make(map[string]bool)
    for _, line := range strings.Split(content, "\n") {
        set[line] = true
    }
    return set
}

func toNormalizedLineSet(content string) map[string]bool {
    set := make(map[string]bool)
    for _, line := range strings.Split(content, "\n") {
        set[strings.TrimSpace(line)] = true
    }
    return set
}
```

### Integration with WriteTemporary

```go
// In checkpoint/temporary.go - modify WriteTemporary

func (s *GitStore) WriteTemporary(ctx context.Context, opts WriteTemporaryOptions) (WriteTemporaryResult, error) {
    // Validate inputs...

    // Get shadow branch name WITH SUFFIX
    shadowBranchName := ShadowBranchNameForCommitWithSuffix(opts.BaseCommit, opts.Suffix)

    // Rest of implementation stays the same...
}

// Update WriteTemporaryOptions
type WriteTemporaryOptions struct {
    BaseCommit      string
    Suffix          int      // NEW: which suffix to use
    SessionID       string
    // ... existing fields
}
```

### Integration with SaveChanges

```go
// In strategy/manual_commit.go - modify SaveChanges

func (s *ManualCommitStrategy) SaveChanges(ctx SaveContext) (*SaveResult, error) {
    // ... existing setup ...

    // Decide which suffix to use
    decision, err := s.DecideSuffix(repo, state.BaseCommit, state.ShadowBranchSuffix)
    if err != nil {
        return nil, fmt.Errorf("failed to decide suffix: %w", err)
    }

    // Update session state if suffix changed
    if decision.Suffix != state.ShadowBranchSuffix {
        logging.Info(logCtx, "shadow branch suffix changed",
            slog.Int("old_suffix", state.ShadowBranchSuffix),
            slog.Int("new_suffix", decision.Suffix),
            slog.String("reason", decision.Reason),
        )
        state.ShadowBranchSuffix = decision.Suffix

        // Reset checkpoint count for new suffix
        if decision.IsNew {
            state.CheckpointCount = 0
            state.FilesTouched = nil
        }
    }

    // Write checkpoint with suffix
    result, err := store.WriteTemporary(ctx.Context, checkpoint.WriteTemporaryOptions{
        BaseCommit:      state.BaseCommit,
        Suffix:          state.ShadowBranchSuffix,  // NEW
        SessionID:       state.SessionID,
        // ... rest of options
    })

    // ... rest of implementation
}
```

### Condensation Changes

```go
// In strategy/manual_commit_condensation.go

func (s *ManualCommitStrategy) CondenseSession(
    repo *git.Repository,
    checkpointID id.CheckpointID,
    state *SessionState,
) (*CondenseResult, error) {
    // Get shadow branch WITH SUFFIX
    shadowBranchName := getShadowBranchNameForCommitWithSuffix(
        state.BaseCommit,
        state.ShadowBranchSuffix,
    )

    // ... rest stays the same, just uses suffixed branch name
}
```

### Cleanup Changes

```go
// In strategy/manual_commit_hooks.go - modify PostCommit cleanup

func (s *ManualCommitStrategy) PostCommit() error {
    // ... existing condensation logic ...

    // Clean up ALL suffixes for this base commit
    if err := s.deleteAllShadowBranchSuffixes(repo, baseCommit); err != nil {
        logging.Warn(logCtx, "failed to clean up shadow branches",
            slog.String("error", err.Error()),
        )
    }

    return nil
}

func (s *ManualCommitStrategy) deleteAllShadowBranchSuffixes(
    repo *git.Repository,
    baseCommit string,
) error {
    hash := baseCommit
    if len(hash) > checkpoint.ShadowBranchHashLength {
        hash = hash[:checkpoint.ShadowBranchHashLength]
    }
    prefix := checkpoint.ShadowBranchPrefix + hash + "-"

    iter, err := repo.Branches()
    if err != nil {
        return err
    }

    var toDelete []plumbing.ReferenceName
    iter.ForEach(func(ref *plumbing.Reference) error {
        if strings.HasPrefix(ref.Name().Short(), prefix) {
            toDelete = append(toDelete, ref.Name())
        }
        return nil
    })

    for _, refName := range toDelete {
        if err := repo.Storer.RemoveReference(refName); err != nil {
            logging.Warn(context.Background(), "failed to delete shadow branch",
                slog.String("branch", refName.Short()),
                slog.String("error", err.Error()),
            )
        }
    }

    return nil
}
```

## Decision Matrix

| Worktree state | Shadow touched files | Agent's lines present | Decision |
|----------------|---------------------|----------------------|----------|
| Clean | - | - | NEW suffix |
| Modified | No overlap | - | NEW suffix |
| Modified | Overlap exists | Yes | SAME suffix |
| Modified | Overlap exists | No | NEW suffix |

## Line Matching Approaches

Two approaches for checking if agent's changes are preserved:

| Approach | Method | Pros | Cons |
|----------|--------|------|------|
| **Exact line matching** | Line exists identically in both shadow and worktree (and not in base) | Precise, no false positives | Fails if user changed indentation or made minor edits |
| **Fuzzy line matching** | Compare lines with whitespace normalized | Tolerates indentation changes | May have false positives on common patterns like `}` or `return nil` |

Recommendation: Start with exact matching. If user feedback indicates too many false "new suffix" decisions, add fuzzy matching as an option.

## Migration

Existing shadow branches (`entire/<hash>` without suffix) should be treated as suffix `0` or migrated to `-1`:

```go
func (s *GitStore) migrateLegacyShadowBranch(baseCommit string) error {
    hash := baseCommit[:ShadowBranchHashLength]
    legacyName := ShadowBranchPrefix + hash
    newName := ShadowBranchPrefix + hash + "-1"

    legacyRef, err := s.repo.Reference(plumbing.NewBranchReferenceName(legacyName), true)
    if err != nil {
        return nil // No legacy branch, nothing to migrate
    }

    // Create new suffixed reference
    newRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(newName), legacyRef.Hash())
    if err := s.repo.Storer.SetReference(newRef); err != nil {
        return err
    }

    // Delete legacy reference
    return s.repo.Storer.RemoveReference(legacyRef.Name())
}
```

## Benefits

- **No ghost files** - Each suffix starts fresh or continues cleanly
- **No content corruption** - Sessions cannot overwrite each other's trees
- **Accurate attribution** - Each suffix has coherent single-stream history
- **No dismiss detection needed** - We detect "should I continue or start fresh" based on observable file state
- **Stash-safe** - Temporary stash operations don't lose checkpoint data

## Open Questions

1. **Fuzzy vs exact matching** - Which provides better user experience?
2. **Multi-suffix condensation** - Should we try to match worktree to any suffix at commit time, or always use the session's stored suffix?
3. **Suffix limit** - Should we cap the number of suffixes to prevent unbounded growth? (Cleanup on commit should prevent this naturally)
