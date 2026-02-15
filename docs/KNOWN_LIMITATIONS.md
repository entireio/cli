# Known Limitations

This document describes known limitations of the Entire CLI.

## Git Operations

### Amending Commits with `-m` Flag

When you amend a commit using `git commit --amend -m "new message"`, the `-m` flag replaces the entire message including any `Entire-Checkpoint` trailer. Git passes `source="message"` (not `"commit"`) to the prepare-commit-msg hook, so the amend-specific trailer preservation logic is bypassed.

**However, the trailer is automatically restored** if `LastCheckpointID` exists in session state (set during the original condensation). This means `git commit --amend -m "..."` preserves the checkpoint link in most cases, including when Claude does the amend in a non-interactive environment.

The only case where the link is lost is when `-m` is used with genuinely *new* content (no prior condensation) and `/dev/tty` is not available for the interactive confirmation prompt.

**Tracked in:** [ENT-161](https://linear.app/entirehq/issue/ENT-161)

### Git GC Can Corrupt Worktree Indexes

When using git worktrees, `git gc --auto` can corrupt a worktree's index by pruning loose objects that the worktree's index cache-tree references. This manifests as:

```
fatal: unable to read <hash>
error: invalid sha1 pointer in cache-tree of .git/worktrees/<n>/index
```

**Root cause:** Checkpoint saves use go-git's `SetEncodedObject` which creates loose objects. When the count exceeds the `gc.auto` threshold (default 6700), any git operation (e.g., VS Code or Sourcetree background fetch) triggers `git gc --auto`. GC doesn't fully account for worktree index references when pruning, so objects get deleted while the worktree index still points to them.

**Impact:**
- `git status` fails in the affected worktree
- Staged changes in the worktree are lost

**Recovery:**
```bash
# In the affected worktree:
git read-tree HEAD
```
This rebuilds the index from HEAD. Any previously staged changes will need to be re-staged.

**Prevention:** Disable auto-GC and run it manually after commits (when indexes are clean):
```bash
git config gc.auto 0
```

**Tracked in:** [ENT-241](https://linear.app/entirehq/issue/ENT-241)
