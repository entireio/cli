# Prompts Index - Implementation Complete

## Overview

The `entire prompts` feature provides offline-first, searchable prompt history from checkpoint data. This document captures the complete implementation.

## CLI Commands

| Command | File | Description |
|---------|------|-------------|
| `entire prompts search [query]` | `search.go` | Full-text search with filters |
| `entire prompts list` | `list.go` | List recent prompts |
| `entire prompts show <checkpoint-id>` | `show.go` | Show full prompt for checkpoint |
| `entire prompts index` | `index_cmd.go` | Index management (rebuild, status) |

## Architecture

### Data Flow

```
Checkpoint Metadata (entire/checkpoints/v1)
        ↓
   Index Builder (walks shards, extracts prompts)
        ↓
Index Store (.entire/prompts/index.ndjson)
        ↓
Search/Rank (tokenize, score, filter)
        ↓
CLI Output (search, list, show)
```

### Index Format

**Location:** `.entire/prompts/index.ndjson` (gitignored)

**Format:** Newline-delimited JSON (appendable, no compression)

```json
{"version":1,"created_at":"2026-05-13T10:00:00Z","repo_root":"/path/to/repo"}
{"checkpoint_id":"a3b2c4d5e6f7","session_index":0,"turn_index":0,"kind":"session","prompt_text":"...","prompt_truncated":false,"commit_hash":"abc123","commit_message":"feat: add search","branch":"main","agent":"Claude Code","model":"haiku","token_count":150,"files_touched":["main.go"],"created_at":"2026-05-13T09:30:00Z"}
```

## Key Decisions

1. **NDJSON over SQLite** - Appendable, no external deps, simple
2. **Porter Stemmer** - Improves recall (caching→cache, authenticated→authent)
3. **NFC Unicode Normalization** - Handles "café" = "cafe\u0301"
4. **Weighted Scoring** - Phrase(+10), all tokens(+5), any token(+1), density(*2)
5. **File Locking** - 3x retry with 50ms backoff, 0o600 permissions
6. **2000 char truncation** - Full text via `show` command
7. **Query guards** - Strip regex metacharacters, min 2 chars

## Algorithms

### Tokenization (rank.go)

```go
func Tokenize(text string) []string {
    // 1. NFC unicode normalization
    // 2. Lowercase
    // 3. Split on word boundaries
    // 4. Remove stopwords (a, an, the, is, etc.)
    // 5. Stem with Porter stemmer
}
```

### Scoring (rank.go)

```
Phrase match: +10 points
All tokens found: +5 points  
Any token found: +1 point
Term density: matches / total_tokens * 2
```

### Filtering (rank.go)

- `--agent`: Filter by agent name
- `--branch`: Filter by branch
- `--kind`: Filter by kind (session, agent_review)
- `--after`: Filter by date (YYYY-MM-DD)
- `--files`: Filter by files touched

### Search Algorithm

1. Parse query: extract phrase (quoted), tokenize remaining
2. For each entry:
   - Skip if filter doesn't match
   - Score using weighted algorithm
   - Keep if score > 0
3. Sort by score descending, then by time
4. Apply limit

## Test Results

**Unit tests:** 16 tests - all passing

| Test | Purpose |
|------|---------|
| TestTokenize_stemming | Verify Porter stemmer |
| TestTokenize_stopwords | Verify stopword removal |
| TestTokenize_unicode | Verify NFC normalization |
| TestTokenize_specialChars | Verify special char handling |
| TestParseQuery_basic | Verify basic query parsing |
| TestParseQuery_phrase | Verify phrase extraction |
| TestParseQuery_specialChars | Verify regex stripping |
| TestParseQuery_tooShort | Verify min length check |
| TestScore_exactPhrase | Verify phrase scoring |
| TestScore_allTokens | Verify all-tokens scoring |
| TestScore_termDensity | Verify density calculation |
| TestSearch_returnsRanked | Verify ranking |
| TestSearch_emptyQuery | Verify empty query handling |
| TestSearch_filters | Verify filter application |

**Benchmarks:**

| Benchmark | Result | Target |
|-----------|--------|--------|
| BenchmarkTokenize | ~0.1ms per call | <1ms ✓ |
| BenchmarkSearch1K (1K entries) | 5.6ms | <100ms ✓ |

**Live testing:**
- 4 checkpoints, 94 prompts indexed
- 98.2 KB index size

## Edge Cases Handled

### Query Edge Cases
- Empty queries return no results
- Queries < 2 chars rejected
- Regex metacharacters stripped (`${}[]()....*+?^|\\`)
- Quoted phrases extracted for exact matching

### Index Edge Cases
- Missing index: auto-rebuild on search
- Corrupt index: rebuild with warning
- Empty index: graceful "no prompts" message
- Concurrent writes: file locking with retry

### Display Edge Cases
- Truncated prompts: "(truncated)" suffix shown
- Ambiguous checkpoint IDs: show disambiguation list
- Missing fields: show available info only

### Search Edge Cases
- Agent filter case-insensitive
- Files filter partial match
- Date filter parses YYYY-MM-DD format
- Zero results: helpful message

## Type Stuttering Fixes

Fixed revive lint errors:

| Old Type | New Type | Reason |
|----------|----------|--------|
| `PromptEntry` | `Entry` | "prompt entry entry" stuttering |
| `IndexStore` | `Store` | "index store store" stuttering |
| `IndexHeader` | `Header` | "index header header" stuttering |
| `IndexStats` | `Stats` | "index stats stats" stuttering |
| `IndexBuilder` | `Builder` | "index builder builder" stuttering |

## Files Modified

```
cmd/entire/cli/prompts/
├── prompts.go          # Added truncatedNoteSuffix constant
├── search.go          # Updated to use NewStore, NewBuilder
├── list.go            # Updated to use NewStore
├── show.go            # Updated to use NewStore, Entry type
├── index_cmd.go       # Updated to use NewStore
└── index/
    ├── schema.go      # Changed PromptEntry → Entry
    ├── rank.go        # Changed PromptEntry → Entry, Entry → Entry
    ├── store.go       # Changed IndexStore → Store, IndexHeader → Header, IndexStats → Stats
    ├── builder.go     # Changed IndexBuilder → Builder, fixed unused header, removed conversions
    ├── update.go      # Changed IndexStore → Store, IndexBuilder → Builder
    └── rank_test.go   # Changed PromptEntry → Entry
```

## Integration

- PostCommit hook triggers index updates via `UpdateIndexForCheckpoint`
- Commands registered in `root.go` via `prompts.NewCommandGroup()`
- Auto-rebuild on missing index during search

## Lint Results

```
[lint:go] 0 issues.
```

All checks pass:
- ✓ gofmt formatting
- ✓ golangci-lint
- ✓ go vet
- ✓ go mod tidy
- ✓ 16 unit tests
- ✓ Build succeeds