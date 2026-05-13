# Entire Prompts Feature - Implementation Documentation

## Overview

This document describes the implementation of the `entire prompts` command - a feature for searchable prompt history from checkpoint data.

## What Was Implemented

### Commands Added

1. **`entire prompts search [query]`** - Search prompts by keywords
   - Filters: `--agent`, `--branch`, `--kind`, `--after`, `--files`
   - Output: `--json` flag for JSON output

2. **`entire prompts list`** - List recent prompts from checkpoint history
   - Flag: `--limit` (default 20)

3. **`entire prompts show <checkpoint-id>`** - Display full prompt for a checkpoint

4. **`entire prompts index`** - Manage the search index
   - `--rebuild`: Rebuild index from scratch
   - `--status`: Show index statistics
   - `--verify`: Verify index entries against git

### Files Created

```
cmd/entire/cli/prompts/
├── prompts.go          # Command group registration
├── list.go             # list command
├── search.go           # search command  
├── show.go             # show command
├── index_cmd.go        # index management command
└── index/
    ├── schema.go       # Data structures (PromptEntry, SearchConfig)
    ├── rank.go         # Tokenizer, stemmer, scoring algorithm
    ├── store.go        # Index I/O with file locking
    ├── builder.go      # Build index from git checkpoint tree
    └── update.go       # Incremental index update function
```

### Files Modified

- `cmd/entire/cli/root.go` - Added prompts command to CLI
- `cmd/entire/cli/strategy/manual_commit_hooks.go` - Integrated index update in PostCommit hook

## Logic Flow

### 1. Index Building (Full Rebuild)

```
entire prompts search "query"
    ↓
Index doesn't exist?
    ↓ Yes
Trigger automatic rebuild
    ↓
builder.Build():
    1. Initialize empty index file
    2. Get HEAD of entire/checkpoints/v1 branch
    3. Walk all checkpoint directories (shard/ID format)
    4. For each checkpoint:
       - Read metadata.json (CheckpointSummary)
       - Read prompt.txt (all prompts)
       - For each session:
         - Read session/metadata.json (CommittedMetadata)
         - Extract prompt from prompt.txt or metadata
         - Create PromptEntry
    5. Write all entries to index.ndjson
```

### 2. Incremental Index Update (PostCommit Hook)

```
User commits with checkpoint trailer
    ↓
strategy.PostCommit() runs
    ↓
condenseAndUpdateState():
    1. Condense session to entire/checkpoints/v1
    2. If successful and has prompts:
       - Get current branch name (git HEAD)
       - Get commit message (first line)
       - Get repo root path
       - For each prompt in result.Prompts:
         - Call index.UpdateIndexForCheckpoint()
    ↓
UpdateIndexForCheckpoint():
    1. Create IndexStore with paths
    2. Create IndexBuilder
    3. AppendCheckpoint():
       - Truncate prompt if > 2000 chars
       - Create PromptEntry with all metadata
       - Acquire file lock (with retry)
       - Append entry to index.ndjson
       - Release lock
```

### 3. Search Query

```
entire prompts search "cache decision"
    ↓
Load index from .entire/prompts/index.ndjson
    ↓
ParseQuery("cache decision"):
    1. Extract quoted phrases (e.g., "cache decision")
    2. Tokenize remaining text
    3. Apply Porter stemmer to each token
    4. Filter stop words
    ↓
For each entry in index:
    1. Check filters (agent, branch, kind, after, files)
    2. If passes filters:
       - Tokenize entry's prompt text
       - Score based on:
         * Exact phrase match (+10)
         * All tokens present (+5)
         * Any token match (+1)
         * Term density bonus (matches/total * 2)
    3. Keep entries with score > 0
    ↓
Sort by score descending, then by date
    ↓
Return top N results (default 20)
```

## Algorithm Details

### Tokenizer (rank.go)

```go
Tokenize(text string) []string:
    1. Lowercase the text
    2. Split on word boundaries ([^\pL\pN]+)
    3. For each token:
       - Skip if length < 2
       - Skip if stop word (a, an, the, is, etc.)
       - Apply Porter stemmer
       - Add to result
    4. Return stemmed tokens
```

**Example:**
- "caching" → "cach"
- "authentication" → "authent"
- "The quick brown fox" → ["quick", "brown", "fox"]

### Scorer (rank.go)

```go
ScoreEntry(entry, query) float64:
    score = 0
    
    // Exact phrase bonus
    if query.Phrase exists and contains in prompt:
        score += 10
    
    // All tokens match
    if all query tokens present in prompt tokens:
        score += 5
    
    // Any token match
    if any query token present:
        score += 1
        matchCount++
    
    // Term density
    if prompt has tokens:
        density = matchCount / len(promptTokens)
        score += density * 2
    
    return score
```

### File Locking (store.go)

```go
- Uses O_CREATE | O_EXCL | O_WRONLY for atomic lock file creation
- Retry up to 3 times with 50ms backoff
- Lock file at .entire/prompts/index.lock
- Automatically cleaned up on Unlock()
```

## Data Structures

### PromptEntry (schema.go)

```go
type PromptEntry struct {
    CheckpointID    string    // 12-char hex ID (e.g., "abc123def456")
    SessionIndex    int       // 0-based session index
    TurnIndex       int       // 0-based turn index
    Kind            string    // "session" or "agent_review"
    PromptText      string    // Truncated to 2000 chars in index
    PromptTruncated bool      // True if was truncated
    CommitHash      string    // SHA of commit with trailer
    CommitMessage   string    // First line of commit message
    Branch          string    // Branch name at commit time
    Agent           string    // Agent type (e.g., "claude-code")
    Model           string    // Model name
    FilesTouched    []string  // Files modified in checkpoint
    CreatedAt       time.Time // When entry was indexed
}
```

### SearchConfig (schema.go)

```go
type SearchConfig struct {
    Query   string  // Search keywords
    Limit   int     // Max results (default 20)
    JSON    bool    // Output as JSON
    Agent   string  // Filter by agent
    Branch  string  // Filter by branch
    Kind    string  // Filter by kind
    After   string  // Filter by date (YYYY-MM-DD)
    Files   string  // Filter by files touched
}
```

## How to Test

### 1. Build Verification

```bash
cd /Users/aasheesh/Documents/webdev/os/cli
go build ./...
```

Expected: No errors

### 2. Command Registration

```bash
go run ./cmd/entire prompts --help
```

Expected: Shows all subcommands (search, list, show, index)

### 3. Empty Index Test

```bash
go run ./cmd/entire prompts search "test"
go run ./cmd/entire prompts list
go run ./cmd/entire prompts index --status
```

Expected: 
- search: "No results for test" or triggers rebuild with "Indexed 0 prompts"
- list: "No prompts found" or triggers rebuild
- status: Shows 0 prompts, index exists

### 4. Integration Test (Requires Checkpoints)

To fully test, you need a repo with actual checkpoints:

```bash
# 1. Enable entire in a repo
entire enable
entire agent add claude-code

# 2. Run some agent sessions and make commits
claude  # or your configured agent
# ... do some work ...
git commit -m "Add feature"

# 3. Test prompts commands
entire prompts search "feature"
entire prompts list
entire prompts index --status
```

Expected: Shows actual prompts from checkpoint history

### 5. Test PostCommit Integration

```bash
# Make a commit with an active session
git commit -m "Test commit"

# Check if prompt was added to index
entire prompts list
```

Expected: New prompt appears in list

## Known Limitations

1. **No unit tests yet** - Need to add tests for tokenizer, scorer, search

2. **Lint warnings** - There are ~50 lint issues in the new code (mostly wrapcheck, gosec, revive)

3. **No incremental update on rebase** - PostRewrite hook doesn't update index

4. **Truncation** - Prompts > 2000 chars are truncated; full text available via git

5. **No index compaction** - Index grows indefinitely; may need periodic rebuild

6. **Branch filtering** - Branch filter uses exact match, not prefix

## Future Improvements

1. Add unit tests for ranking algorithm
2. Add benchmark tests for search performance (<100ms for 1K checkpoints)
3. Implement index compaction/rebuild
4. Add fuzzy matching for typo tolerance
5. Support for searching code changes (not just prompts)
6. Add pagination for large result sets

## Architecture Diagram

```
┌─────────────────────────────────────────────────────────────┐
│                     User Commands                           │
├─────────────────────────────────────────────────────────────┤
│  entire prompts search <query>                              │
│  entire prompts list                                         │
│  entire prompts show <id>                                    │
│  entire prompts index --status                               │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                     prompts package                          │
├─────────────────────────────────────────────────────────────┤
│  prompts/search.go                                           │
│    ├── Load index (store.Load)                              │
│    ├── Parse query (rank.ParseQuery)                        │
│    ├── Search (rank.Search)                                  │
│    └── Format results                                        │
│                                                               │
│  prompts/index/                                              │
│    ├── store.go: Index I/O + locking                        │
│    ├── rank.go: Tokenization + scoring                      │
│    └── builder.go: Build from git tree                      │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                   .entire/prompts/                          │
├─────────────────────────────────────────────────────────────┤
│  index.ndjson  (Appendable JSON lines, gitignored)          │
│  index.lock    (File lock for concurrent access)           │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                  Git Checkpoint Data                         │
├─────────────────────────────────────────────────────────────┤
│  entire/checkpoints/v1/                                      │
│  ├── <shard>/<id>/0/                                        │
│  │   ├── metadata.json (CheckpointSummary)                 │
│  │   ├── prompt.txt (all prompts, split by ---\n\n)        │
│  │   └── 0/                                                 │
│  │       └── metadata.json (CommittedMetadata)              │
│  └── ...                                                     │
└─────────────────────────────────────────────────────────────┘
```

## Key Design Decisions

1. **NDJSON format** - Appendable, simple, no compression overhead
2. **Porter stemmer** - Better recall (caching→cache, authenticated→authent)
3. **File locking** - Safe for concurrent PostCommit hook access
4. **2000 char truncation** - Balance between index size and searchability
5. **Location-independent** - Index uses relative paths, works after repo relocation
6. **Graceful degradation** - Index errors don't fail commits, just log warnings