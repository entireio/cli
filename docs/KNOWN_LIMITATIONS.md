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

**Root cause:** Checkpoint writes at commit time use go-git's `SetEncodedObject`, which creates loose objects. (Turn ends no longer write git objects; older versions also wrote them to shadow branches on every turn.) When the count exceeds the `gc.auto` threshold (default 6700), any git operation (e.g., VS Code or Sourcetree background fetch) triggers `git gc --auto`. GC doesn't fully account for worktree index references when pruning, so objects get deleted while the worktree index still points to them.

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

### Concurrent ACTIVE Sessions May Produce Spurious Checkpoints

When multiple sessions are ACTIVE in the same directory and one session's agent (or subagent) makes a commit, **all** ACTIVE sessions are condensed — including sessions that didn't contribute to the commit. This can produce checkpoint entries with minimal content (e.g., just the initial prompt) linked to a commit the session didn't work on.

**Why:** PostCommit sets `hasNew=true` for all ACTIVE sessions. We cannot re-validate via transcript analysis because subagent file modifications live in a separate transcript from the main agent's. Attempting to re-validate causes subagent commits to skip condensation entirely (data loss), which is worse than a spurious empty checkpoint.

**Impact:** Cosmetic — extra metadata entries on `entire/checkpoints/v1` with minimal content. No data loss or corruption.

**Workaround:** Use separate git worktrees for concurrent sessions. A commit condenses the sessions with pending work in its own worktree (plus any session whose agent process made the commit), so sessions in different worktrees don't interfere.

### Shell Edits During Overlapping Turns Are Not Attributed

When sessions share a worktree and their turns overlap, a file that only `git status` shows as changed (an edit made through a shell command, which no transcript names) is recorded by neither session, so a commit of only that file gets no checkpoint. Files an agent changes with its file-editing tools are named in its transcript and are attributed as usual. Another session that stops during the turn (its agent is closed) counts as overlapping too, because the check reads the session's last interaction time, which a stop also updates.

On Windows, Entire cannot yet check whether a session's agent process is still running, or which process made a commit, so two cases differ there. A session that has been mid-turn for over an hour does not count as overlapping, and the other session takes the files only `git status` shows, as before. And a commit of such a file made outside any agent (a script or a GUI client) while another session is mid-turn links that session, as a commit of any file no session recorded does there.

**Why:** `git status` shows every dirty file in the worktree, not which session changed it. Taking such a file would link a session to another session's commit, and `entire checkpoint explain` would show that session's prompt and transcript as the commit's intent.

**Workaround:** Use separate git worktrees for concurrent sessions, as above.

## Upgrades and Downgrades

### Downgrading Discards Pending Agent Work

Commit any pending agent work before downgrading to a version that still writes shadow branches, or before running such a version (for example an editor-bundled or pinned `entire`) against the same clone.

**Why:** Current versions record pending work in session state only and never write the `entire/<commit>-<worktree>` shadow branch. Older versions delete any ended, never-condensed session state whose shadow branch is missing, which they check on every `prepare-commit-msg` and `post-commit`. Every ended session with uncommitted agent work matches, so the older hook deletes its state before linking: the commit gets no trailer and no checkpoint.

**Workaround:** Commit (or discard) the agent's work with the current version first; `entire checkpoint list --pending` shows what is still pending.
