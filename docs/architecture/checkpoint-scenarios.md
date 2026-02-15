# Checkpoint Scenarios

This document describes how the one-to-one checkpoint system handles various user workflows.

## Overview

The system uses:
- **Shadow branches** (`entire/<commit-hash>-<worktree-hash>`) - temporary storage for checkpoint data
- **FilesTouched** - accumulates files modified during the session
- **1:1 checkpoints** - each commit gets its own unique checkpoint ID
- **Content-aware overlap** - prevents linking commits where user reverted session changes

## State Machine

```mermaid
stateDiagram-v2
    [*] --> IDLE : SessionStart

    IDLE --> ACTIVE : TurnStart (UserPromptSubmit)
    ACTIVE --> IDLE : TurnEnd (Stop hook)
    ACTIVE --> ACTIVE : GitCommit / Condense
    IDLE --> IDLE : GitCommit / Condense

    IDLE --> ENDED : SessionStop
    ACTIVE --> ENDED : SessionStop
    ENDED --> ACTIVE : TurnStart (session resume)
    ENDED --> ENDED : GitCommit / CondenseIfFilesTouched
```

---

## Scenario 1: Prompt → Changes → Prompt Finishes → User Commits

The simplest workflow: user runs a prompt, Claude makes changes, prompt finishes, then user manually commits.

```mermaid
sequenceDiagram
    participant U as User
    participant C as Claude
    participant G as Git Hooks
    participant S as Session State
    participant SB as Shadow Branch

    U->>C: Submit prompt
    Note over G: UserPromptSubmit hook
    G->>S: InitializeSession (IDLE→ACTIVE)
    S->>S: TurnID generated

    C->>C: Makes changes (A, B, C)
    C->>G: SaveChanges (Stop hook)
    G->>SB: Write checkpoint (A, B, C + transcript)
    G->>S: FilesTouched = [A, B, C]
    Note over G: TurnEnd: ACTIVE→IDLE

    Note over U: Later...
    U->>G: git commit -a
    Note over G: PrepareCommitMsg hook
    G->>S: Check FilesTouched [A, B, C]
    G->>G: staged [A,B,C] ∩ FilesTouched → overlap ✓
    G->>G: Generate checkpoint ID, add trailer

    Note over G: PostCommit hook
    G->>G: EventGitCommit (IDLE)
    G->>SB: Condense to entire/checkpoints/v1
    G->>SB: Delete shadow branch
    G->>S: FilesTouched = nil
```

### Key Points
- Shadow branch holds checkpoint data until user commits
- PrepareCommitMsg adds `Entire-Checkpoint` trailer
- PostCommit condenses to permanent storage and cleans up

---

## Scenario 2: Prompt Commits Within Single Turn

Claude is instructed to commit changes, so the commit happens during the ACTIVE phase.

```mermaid
sequenceDiagram
    participant U as User
    participant C as Claude
    participant G as Git Hooks
    participant S as Session State
    participant SB as Shadow Branch

    U->>C: "Make changes and commit them"
    Note over G: UserPromptSubmit hook
    G->>S: InitializeSession (→ACTIVE)

    C->>C: Makes changes (A, B)
    C->>G: git add && git commit

    Note over G: PrepareCommitMsg (no TTY = agent commit)
    G->>G: Generate checkpoint ID, add trailer directly

    Note over G: PostCommit hook (ACTIVE)
    G->>G: EventGitCommit (ACTIVE→ACTIVE)
    G->>SB: Condense with provisional transcript
    G->>S: TurnCheckpointIDs += [checkpoint-id]
    G->>S: FilesTouched = nil

    C->>G: Responds with summary
    Note over G: Stop hook
    G->>G: HandleTurnEnd (ACTIVE→IDLE)
    G->>G: UpdateCommitted: finalize with full transcript
    G->>S: TurnCheckpointIDs = nil
```

### Key Points
- Agent commits detected by no TTY → fast path adds trailer directly
- **Deferred finalization**: PostCommit saves provisional transcript, HandleTurnEnd updates with full transcript
- TurnCheckpointIDs tracks mid-turn checkpoints for finalization at stop

---

## Scenario 3: Claude Makes Multiple Granular Commits

Claude is instructed to make granular commits, resulting in multiple commits during one turn.

```mermaid
sequenceDiagram
    participant U as User
    participant C as Claude
    participant G as Git Hooks
    participant S as Session State

    U->>C: "Implement feature with granular commits"
    Note over G: UserPromptSubmit → ACTIVE

    C->>C: Creates file A
    C->>G: git commit -m "Add A"
    Note over G: PrepareCommitMsg: checkpoint-1
    Note over G: PostCommit (ACTIVE)
    G->>G: Condense checkpoint-1 (provisional)
    G->>S: TurnCheckpointIDs = [checkpoint-1]

    C->>C: Creates file B
    C->>G: git commit -m "Add B"
    Note over G: PrepareCommitMsg: checkpoint-2
    Note over G: PostCommit (ACTIVE)
    G->>G: Condense checkpoint-2 (provisional)
    G->>S: TurnCheckpointIDs = [checkpoint-1, checkpoint-2]

    C->>C: Creates file C
    C->>G: git commit -m "Add C"
    Note over G: PrepareCommitMsg: checkpoint-3
    Note over G: PostCommit (ACTIVE)
    G->>G: Condense checkpoint-3 (provisional)
    G->>S: TurnCheckpointIDs = [checkpoint-1, checkpoint-2, checkpoint-3]

    C->>G: Summary response
    Note over G: Stop hook → HandleTurnEnd
    G->>G: Finalize ALL checkpoints with full transcript
    Note right of G: checkpoint-1: UpdateCommitted<br/>checkpoint-2: UpdateCommitted<br/>checkpoint-3: UpdateCommitted
    G->>S: TurnCheckpointIDs = nil
```

### Key Points
- Each commit gets its own unique checkpoint ID (1:1 model)
- All checkpoints are finalized together at turn end
- Each checkpoint has the full session transcript for context

---

## Scenario 4: User Splits Changes Into Multiple Commits

User decides to create multiple commits from Claude's changes after the prompt finishes.

```mermaid
sequenceDiagram
    participant U as User
    participant C as Claude
    participant G as Git Hooks
    participant S as Session State
    participant SB as Shadow Branch

    U->>C: Submit prompt
    Note over G: UserPromptSubmit → ACTIVE

    C->>C: Makes changes (A, B, C, D)
    C->>G: SaveChanges (Stop hook)
    G->>SB: Write checkpoint (A, B, C, D)
    G->>S: FilesTouched = [A, B, C, D]
    Note over G: TurnEnd: ACTIVE→IDLE

    Note over U: User commits A, B only
    U->>G: git add A B && git commit
    Note over G: PrepareCommitMsg: checkpoint-1
    Note over G: PostCommit (IDLE)
    G->>G: committedFiles = {A, B}
    G->>G: remaining = [C, D]
    G->>SB: Condense checkpoint-1
    G->>SB: Carry-forward C, D to new shadow branch
    G->>S: FilesTouched = [C, D]

    Note over U: User commits C, D
    U->>G: git add C D && git commit
    Note over G: PrepareCommitMsg: checkpoint-2
    Note over G: PostCommit (IDLE)
    G->>G: committedFiles = {C, D}
    G->>G: remaining = []
    G->>SB: Condense checkpoint-2
    G->>S: FilesTouched = nil
```

### Key Points
- **Carry-forward logic**: uncommitted files get a new shadow branch
- Each commit gets its own checkpoint ID (1:1 model)
- Both checkpoints link to the same session transcript

### Content-Aware Carry-Forward

The carry-forward logic uses **content-aware comparison** to determine which files have remaining uncommitted changes:

1. **File not in commit** → definitely has remaining changes
2. **File in commit, hash matches shadow branch** → fully committed, no carry-forward
3. **File in commit, hash differs from shadow branch** → partial commit (e.g., `git add -p`), carry forward

This enables splitting changes within a single file across multiple commits (see Scenario 7).

---

## Scenario 5: Partial Commit → Stash → Next Prompt

User commits some changes, stashes the rest, then runs another prompt.

```mermaid
sequenceDiagram
    participant U as User
    participant C as Claude
    participant G as Git Hooks
    participant S as Session State
    participant SB as Shadow Branch

    U->>C: Prompt 1
    Note over G: UserPromptSubmit → ACTIVE

    C->>C: Makes changes (A, B, C)
    C->>G: Stop hook
    G->>SB: Checkpoint (A, B, C)
    G->>S: FilesTouched = [A, B, C]
    Note over G: ACTIVE→IDLE

    Note over U: User commits A only
    U->>G: git add A && git commit
    Note over G: PostCommit
    G->>SB: Condense checkpoint-1
    G->>SB: Carry-forward B, C
    G->>S: FilesTouched = [B, C]

    Note over U: User stashes B, C
    U->>U: git stash
    Note right of U: B, C removed from working directory<br/>FilesTouched still = [B, C]

    U->>C: Prompt 2
    Note over G: UserPromptSubmit (IDLE→ACTIVE)
    G->>S: TurnID = new, TurnCheckpointIDs = nil
    Note right of G: FilesTouched NOT cleared<br/>(accumulates across prompts)

    C->>C: Makes changes (D, E)
    C->>G: SaveChanges
    G->>SB: Add D, E to shadow branch tree
    Note right of SB: Tree now has: B, C (old) + D, E (new)
    G->>S: FilesTouched = merge([B,C], [D,E]) = [B,C,D,E]
    Note over G: ACTIVE→IDLE

    Note over U: User commits D, E
    U->>G: git add D E && git commit
    Note over G: PrepareCommitMsg
    G->>G: staged [D,E] ∩ FilesTouched [B,C,D,E] → D,E match ✓
    G->>G: checkpoint-2 trailer added

    Note over G: PostCommit
    G->>G: committedFiles = {D, E}
    G->>G: remaining = [B, C]
    G->>SB: Condense checkpoint-2
    Note right of G: Checkpoint has FULL session transcript<br/>(both Prompt 1 and Prompt 2)

    G->>G: Carry-forward attempt for B, C
    Note right of G: B, C don't exist on disk (stashed)<br/>→ removed from tree
    G->>S: FilesTouched = [B, C]
```

### Key Points
- **FilesTouched accumulates** across prompts (not cleared at TurnStart)
- **Checkpoints have full session context**: D, E commit links to transcript showing BOTH prompts
- **No wrong attribution**: Looking at checkpoint-2, you can see D, E were created by Prompt 2

### Edge Case: Stashed Files Lose Shadow Content

After user commits D, E, the carry-forward for B, C creates an "empty" checkpoint:
- `buildTreeWithChanges` removes non-existent files (B, C are stashed) from the tree
- A shadow branch commit is created, but its tree is just HEAD (no B, C content)
- `FilesTouched` is set to `[B, C]` - the files are still **tracked by name**

**If user later unstashes B, C and commits them:**
- PrepareCommitMsg: staged [B, C] overlaps with FilesTouched [B, C] by filename → trailer added ✓
- PostCommit: checkpoint is created and linked
- But the shadow branch doesn't have the original B, C content from Prompt 1

This is acceptable behavior - stashing files mid-session and committing other files first is an explicit user action. The files are still tracked, but the shadow branch content chain is broken.

---

## Scenario 6: Stash → Second Prompt → Unstash → Commit All

User stashes files, runs another prompt, then unstashes and commits everything together.

```mermaid
sequenceDiagram
    participant U as User
    participant C as Claude
    participant G as Git Hooks
    participant S as Session State
    participant SB as Shadow Branch

    U->>C: Prompt 1
    Note over G: UserPromptSubmit → ACTIVE

    C->>C: Makes changes (A, B, C)
    C->>G: Stop hook
    G->>SB: Checkpoint (A, B, C)
    G->>S: FilesTouched = [A, B, C]
    Note over G: ACTIVE→IDLE

    Note over U: User commits A only
    U->>G: git add A && git commit
    Note over G: PostCommit
    G->>SB: Condense checkpoint-1
    G->>SB: Carry-forward B, C
    G->>S: FilesTouched = [B, C]

    Note over U: User stashes B, C
    U->>U: git stash
    Note right of U: B, C removed from working directory<br/>Shadow branch still has B, C

    U->>C: Prompt 2
    Note over G: UserPromptSubmit (IDLE→ACTIVE)

    C->>C: Makes changes (D, E)
    C->>G: Stop hook (SaveChanges)
    G->>SB: Add D, E to existing shadow branch
    Note right of SB: Tree: B, C (from base) + D, E (new)
    G->>S: FilesTouched = merge([B,C], [D,E]) = [B,C,D,E]
    Note over G: ACTIVE→IDLE

    Note over U: User unstashes B, C
    U->>U: git stash pop
    Note right of U: B, C back in working directory

    Note over U: User commits ALL files
    U->>G: git add B C D E && git commit
    Note over G: PrepareCommitMsg
    G->>G: staged [B,C,D,E] ∩ FilesTouched [B,C,D,E] → all match ✓
    G->>G: checkpoint-2 trailer added

    Note over G: PostCommit
    G->>G: committedFiles = {B, C, D, E}
    G->>G: remaining = []
    G->>SB: Condense checkpoint-2
    Note right of G: Checkpoint includes ALL files (B,C,D,E)<br/>and FULL transcript (both prompts)
    G->>S: FilesTouched = nil
```

### Key Points
- **Shadow branch accumulates**: D, E added on top of existing B, C from carry-forward
- **All files tracked**: When user commits all together, all four files link to checkpoint
- **Full session context**: Checkpoint transcript shows Prompt 1 created B, C and Prompt 2 created D, E

### Contrast with Scenario 5

| Scenario | User Action | Result |
|----------|-------------|--------|
| **5**: Commit D, E first, then B, C later | Commits D, E while B, C stashed | B, C "fall out" - carry-forward fails, later commit of B, C has no shadow content |
| **6**: Commit all together after unstash | Unstashes B, C, commits B, C, D, E together | All files linked to single checkpoint |

The key difference is **when the commit happens relative to the unstash**:
- If you commit while files are stashed → those files lose their shadow branch content
- If you unstash first, then commit → all files are preserved together

---

## Scenario 7: Partial Staging with `git add -p`

User uses interactive staging to commit only some hunks of a file, leaving other agent changes uncommitted.

```mermaid
sequenceDiagram
    participant U as User
    participant C as Claude
    participant G as Git Hooks
    participant S as Session State
    participant SB as Shadow Branch

    U->>C: Submit prompt
    Note over G: UserPromptSubmit → ACTIVE

    C->>C: Makes multiple changes to file A
    Note right of C: A now has lines 1-100<br/>(was empty before)
    C->>G: Stop hook
    G->>SB: Checkpoint (A with lines 1-100)
    G->>S: FilesTouched = [A]
    Note over G: ACTIVE→IDLE

    Note over U: User stages partial content
    U->>G: git add -p A
    Note right of U: Stages only lines 1-50<br/>Worktree still has 1-100

    U->>G: git commit
    Note over G: PrepareCommitMsg: checkpoint-1

    Note over G: PostCommit
    G->>G: committedFiles = {A}
    G->>G: Content check: committed A (lines 1-50)
    G->>G: Shadow A hash ≠ committed A hash
    G->>G: remaining = [A] (has uncommitted changes)
    G->>SB: Condense checkpoint-1
    G->>SB: Carry-forward A to new shadow branch
    Note right of SB: New shadow has A with<br/>current worktree (lines 1-100)
    G->>S: FilesTouched = [A]

    Note over U: User commits remaining
    U->>G: git add A && git commit
    Note over G: PrepareCommitMsg: checkpoint-2

    Note over G: PostCommit
    G->>G: Content check: committed A == shadow A
    G->>G: remaining = []
    G->>SB: Condense checkpoint-2
    G->>S: FilesTouched = nil
```

### Key Points
- **Content-aware carry-forward**: Compares git blob hashes, not just filenames
- Partial staging (`git add -p`) within a single file is detected
- Each commit gets proper attribution, even when splitting one file's changes

### How Content Comparison Works

```mermaid
flowchart TD
    A[PostCommit: Carry-forward check] --> B{File in committedFiles?}
    B -->|No| C[✓ Add to remaining<br/>File not committed at all]
    B -->|Yes| D[Get shadow branch file hash]
    D --> E{Shadow file exists?}
    E -->|No| F[Skip file<br/>Nothing to compare against]
    E -->|Yes| G{Committed hash == shadow hash?}
    G -->|Yes| H[Skip file<br/>Fully committed]
    G -->|No| I[✓ Add to remaining<br/>Partial commit detected]

    C --> J[Carry forward remaining files]
    I --> J
```

---

## Content-Aware Overlap Detection

Prevents linking commits where user reverted session changes and wrote different content.

```mermaid
flowchart TD
    A[PostCommit: Check files overlap] --> B{File in commit AND in FilesTouched?}
    B -->|No| Z[No checkpoint trailer]
    B -->|Yes| C{File existed in parent commit?}
    C -->|Yes: Modified file| D[✓ Counts as overlap<br/>User edited session's work]
    C -->|No: New file| E{Content hash matches shadow branch?}
    E -->|Yes| F[✓ Counts as overlap<br/>Session's content preserved]
    E -->|No| G[✗ No overlap<br/>User reverted & replaced]

    D --> H[Add checkpoint trailer]
    F --> H
    G --> Z
```

### Example: Reverted and Replaced

```mermaid
sequenceDiagram
    participant U as User
    participant C as Claude
    participant G as Git Hooks

    C->>C: Creates file X with content "hello"
    Note over G: Shadow branch: X (hash: abc123)

    U->>U: Reverts: git checkout -- X
    U->>U: Writes completely different content
    Note right of U: X now has content "world"<br/>(hash: def456)

    U->>G: git add X && git commit
    Note over G: PrepareCommitMsg
    G->>G: X in FilesTouched? Yes
    G->>G: X is new file (not in parent)
    G->>G: Compare hashes: abc123 ≠ def456
    G->>G: Content mismatch → NO overlap
    Note over G: No Entire-Checkpoint trailer added
```

---

## Summary Table

| Scenario | When Checkpoint Created | Checkpoint Contains | Key Mechanism |
|----------|------------------------|---------------------|---------------|
| 1. User commits after prompt | PostCommit (IDLE) | Full transcript | Normal condensation |
| 2. Claude commits in turn | PostCommit (ACTIVE) + HandleTurnEnd | Full transcript (finalized at stop) | Deferred finalization |
| 3. Multiple Claude commits | Each PostCommit (ACTIVE) + HandleTurnEnd | Full transcript per checkpoint | TurnCheckpointIDs tracking |
| 4. User splits commits | Each PostCommit (IDLE) | Full transcript per checkpoint | Content-aware carry-forward |
| 5. Partial commit + stash + new prompt + commit new | PostCommit (IDLE) | Full transcript (both prompts) | FilesTouched accumulation, stashed files "fall out" |
| 6. Stash + new prompt + unstash + commit all | PostCommit (IDLE) | All files + full transcript | Shadow branch accumulation |
| 7. Partial staging with `git add -p` | Each PostCommit (IDLE) | Full transcript per checkpoint | Content-aware carry-forward (hash comparison) |

---

## Known Caveats

### 1. Redundant Transcript Data Across Commits

Each checkpoint stores the **full session transcript** up to that point. If a session results in multiple commits (Scenarios 3, 4, 5, 6), each checkpoint contains overlapping transcript data.

**Example**: Session with 3 commits
- Checkpoint 1: transcript lines 1-100
- Checkpoint 2: transcript lines 1-200 (includes 1-100 again)
- Checkpoint 3: transcript lines 1-300 (includes 1-200 again)

**Trade-off**: This simplifies checkpoint retrieval (each is self-contained) at the cost of storage efficiency.

### 2. Token Usage Sums Are Misleading

Each checkpoint's `metadata.json` contains cumulative token usage for the entire session up to that point. Summing token counts across multiple checkpoints from the same session **double-counts tokens**.

**Example**:
- Checkpoint 1: 10,000 tokens (session total so far)
- Checkpoint 2: 25,000 tokens (session total so far)
- Naive sum: 35,000 tokens ❌
- Actual usage: 25,000 tokens ✓

**Correct approach**: Use the token count from the **last checkpoint** of a session, or track incremental deltas separately.

### 3. Stashed Files Lose Shadow Content

As described in Scenario 5, if files are stashed and other files are committed first, the stashed files lose their content in the shadow branch. They remain tracked by filename in `FilesTouched`, but subsequent checkpoints won't have the original file content preserved.

### 4. No Per-File Prompt Attribution

Checkpoints don't explicitly tag which prompt created which file. To determine this, you must parse the transcript and correlate `tool_use` entries with preceding `user` messages. The `files_touched` list in metadata is cumulative across all prompts.

### 5. Carry-Forward Checkpoints Include Full Transcript

When files are carried forward (Scenario 4), `CheckpointTranscriptStart` is reset to 0. This means each carry-forward checkpoint includes the **entire transcript**, not just new content since the last checkpoint.

**Impact**: For long sessions with many partial commits, checkpoint storage grows linearly with session length × number of commits.

### 6. Crash Before HandleTurnEnd Leaves Provisional Transcripts

In Scenarios 2 and 3 (Claude commits during turn), checkpoints are saved with "provisional" transcripts during PostCommit. The full transcript is written at HandleTurnEnd (Stop hook).

If the session crashes or is killed before the Stop hook fires:
- Checkpoints exist with partial transcripts
- `TurnCheckpointIDs` in session state tracks which need finalization
- Next session start does **not** automatically finalize orphaned checkpoints

### 7. Two Different Content-Aware Checks

The system uses two separate content-aware checks with different purposes:

**A. Overlap Detection** (`filesOverlapWithContent`) - Determines if commit should be linked to session:
- Only applies to **newly created files**
- Modified files (existed in parent) **always count as overlap**
- Used in PrepareCommitMsg/PostCommit for non-ACTIVE sessions
- **Purpose**: Prevent linking commits where user reverted session content

**B. Carry-Forward Detection** (`filesWithRemainingAgentChanges`) - Determines which files to carry forward:
- Applies to **all committed files**
- Compares committed content hash vs shadow branch hash
- Hash mismatch = partial commit, file carried forward
- **Purpose**: Enable splitting changes within a file across commits (Scenario 7)

### 8. Carry-Forward Content Superseded by New Prompts

When files are carried forward and then a new prompt modifies the same file:
- The shadow branch gets the **new** content (from the new prompt's SaveChanges)
- The carried-forward content is overwritten
- Subsequent commits compare against the **new prompt's content**, not the original carried-forward content

**Example**:
1. Prompt 1: Agent writes 100 lines to file A
2. User commits 50 lines via `git add -p`
3. Carry-forward: A (with 100 lines) goes to new shadow branch
4. Prompt 2: Agent adds 50 more lines to A (now 150 lines total in worktree)
5. SaveChanges: Shadow branch now has A with 150 lines
6. User commits: Comparison is against 150 lines, not original 100 lines

This is correct behavior - the shadow branch reflects the **current combined state** of the session's work.

### 9. Automatic Cleanup During Normal Operations

Most orphaned data is cleaned up automatically:

- **Shadow branches**: Deleted after condensation if no other sessions reference them
- **Session states**: Cleaned up during session listing when shadow branch no longer exists (and session is not ACTIVE, has no `LastCheckpointID`)

For anything that slips through, run `entire clean` manually:

```bash
entire clean          # Preview orphaned items
entire clean --force  # Delete orphaned items
```
