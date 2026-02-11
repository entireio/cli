#!/bin/bash
set -e

#
# migrate-sessions.sh - Migrate checkpoint data to v1 format
#
# USAGE:
#   ./scripts/migrate-sessions.sh [OPTIONS] [CHECKPOINT_ID]
#
# OPTIONS:
#   -h, --help      Show this help message
#   --apply         Actually perform the migration (default is dry-run)
#
# ARGUMENTS:
#   CHECKPOINT_ID   Optional. Migrate only this checkpoint (e.g., "a1b2c3d4e5f6")
#                   If omitted, migrates all checkpoints from entire/sessions-legacy.
#
# DESCRIPTION:
#   Migrates checkpoint data from the old format (latest session at root, archived
#   sessions in numbered folders 1/, 2/, etc.) to the new v1 format (all sessions
#   in 0-indexed folders 0/, 1/, 2/, with a CheckpointSummary at the root).
#
#   The script reads from 'entire/sessions-legacy' and writes to 'entire/checkpoints/v1',
#   leaving the original branch untouched as a backup.
#
#   By default, runs in dry-run mode showing what would be migrated.
#   Use --apply to actually perform the migration.
#
#   The script is idempotent - checkpoints already migrated to v1 are skipped.
#   This allows running migration incrementally as new checkpoints are added.
#
# OLD FORMAT:
#   <checkpoint-id[:2]>/<checkpoint-id[2:]>/
#   ├── metadata.json      # Session metadata (has session_id)
#   ├── full.jsonl         # Latest session transcript
#   ├── prompt.txt
#   ├── context.md
#   └── 1/                  # Archived session
#       └── ...
#
# NEW FORMAT (v1):
#   <checkpoint-id[:2]>/<checkpoint-id[2:]>/
#   ├── metadata.json      # CheckpointSummary (aggregated stats + session paths)
#   ├── 0/                  # First session (was at root)
#   │   ├── metadata.json  # Session-specific metadata
#   │   ├── full.jsonl
#   │   └── ...
#   └── 1/                  # Second session (was 1/)
#       └── ...
#
# PREREQUISITES:
#   - jq (JSON processor) must be installed
#   - Clean working tree (no uncommitted changes)
#   - The entire/sessions-legacy branch must exist (renamed from entire/sessions)
#   - DISABLE ALL YOUR ENTIRE GIT HOOKS FIRST (e.g., pre-commit, pre-push) to avoid issues during migration
#
# EXAMPLES:
#   # Preview what would be migrated (dry-run)
#   ./scripts/migrate-sessions.sh
#
#   # Migrate all checkpoints
#   ./scripts/migrate-sessions.sh --apply
#
#   # Preview a single checkpoint
#   ./scripts/migrate-sessions.sh a1b2c3d4e5f6
#
#   # Migrate a single checkpoint
#   ./scripts/migrate-sessions.sh a1b2c3d4e5f6 --apply
#
# BEFORE MIGRATION:
#   1. Rename local entire/sessions to entire/sessions-legacy:
#      git branch -m entire/sessions entire/sessions-legacy
#
# AFTER MIGRATION:
#   1. Verify the migration:
#      git log entire/checkpoints/v1
#      git show entire/checkpoints/v1:<checkpoint_path>/metadata.json
#
#   2. Rename remote entire/sessions to avoid conflict:
#      git push origin entire/sessions:entire/sessions-old
#      git push origin --delete entire/sessions
#
#   3. Push new v1 branch to remote:
#      git push origin entire/checkpoints/v1
#
#   4. (Optional) Delete local legacy branch:
#      git branch -D entire/sessions-legacy
#
# ROLLBACK:
#   The original branch is preserved as entire/sessions-legacy locally.
#   If migration fails, delete the v1 branch and rename back:
#      git branch -D entire/checkpoints/v1
#      git branch -m entire/sessions-legacy entire/sessions
#

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

SOURCE_BRANCH="entire/sessions-legacy"
TARGET_BRANCH="entire/checkpoints/v1"

# Parse arguments
DRY_RUN=true
CHECKPOINT_FILTER=""

show_help() {
    sed -n '3,/^$/p' "$0" | sed 's/^# \?//'
    exit 0
}

while [[ $# -gt 0 ]]; do
    case $1 in
        -h|--help)
            show_help
            ;;
        --apply)
            DRY_RUN=false
            shift
            ;;
        -*)
            echo -e "${RED}Unknown option: $1${NC}" >&2
            echo "Use --help for usage information" >&2
            exit 1
            ;;
        *)
            if [[ -z "$CHECKPOINT_FILTER" ]]; then
                CHECKPOINT_FILTER="$1"
            else
                echo -e "${RED}Too many arguments${NC}" >&2
                echo "Use --help for usage information" >&2
                exit 1
            fi
            shift
            ;;
    esac
done

# Check prerequisites
if ! command -v jq &> /dev/null; then
    echo -e "${RED}Error: jq is required but not installed${NC}" >&2
    echo "Install with: brew install jq (macOS) or apt-get install jq (Linux)" >&2
    exit 1
fi

if ! git rev-parse --is-inside-work-tree &> /dev/null; then
    echo -e "${RED}Error: Not inside a git repository${NC}" >&2
    exit 1
fi

if ! git show-ref --verify --quiet "refs/heads/$SOURCE_BRANCH"; then
    echo -e "${RED}Error: Branch '$SOURCE_BRANCH' does not exist${NC}" >&2
    exit 1
fi

if [[ "$DRY_RUN" == "false" ]] && [[ -n $(git status --porcelain) ]]; then
    echo -e "${RED}Error: Working tree is not clean${NC}" >&2
    echo "Please commit or stash your changes first" >&2
    exit 1
fi

echo -e "${GREEN}=== Checkpoint Migration Script ===${NC}"
echo "Source: $SOURCE_BRANCH"
echo "Target: $TARGET_BRANCH"
if [[ -n "$CHECKPOINT_FILTER" ]]; then
    echo "Filter: checkpoint $CHECKPOINT_FILTER only"
fi
if [[ "$DRY_RUN" == "true" ]]; then
    echo -e "${YELLOW}DRY RUN - no changes will be made (use --apply to migrate)${NC}"
fi
echo ""

# Save current branch
ORIGINAL_BRANCH=$(git branch --show-current)

# Convert checkpoint ID to path pattern (e.g., "a1b2c3d4e5f6" -> "a1/b2c3d4e5f6")
checkpoint_to_path() {
    local id="$1"
    echo "${id:0:2}/${id:2}"
}

# Retry a git command with exponential backoff (handles index.lock race conditions)
# Args: $@ = command to run
git_retry() {
    local max_attempts=5
    local attempt=1
    local wait_time=0.2

    while [[ $attempt -le $max_attempts ]]; do
        if "$@" 2>&1; then
            return 0
        fi

        if [[ $attempt -lt $max_attempts ]]; then
            echo "    Retrying in ${wait_time}s (attempt $attempt/$max_attempts)..." >&2
            sleep "$wait_time"
            wait_time=$(echo "$wait_time * 2" | bc)
        fi
        ((attempt++))
    done

    echo "    Failed after $max_attempts attempts" >&2
    return 1
}

# Check if a checkpoint is up-to-date on target branch (same source commit)
# Args: $1 = checkpoint path, $2 = source commit hash
# Returns 0 if exists and up-to-date, 1 otherwise
checkpoint_up_to_date_on_target() {
    local checkpoint_path="$1"
    local source_commit="$2"

    if ! git show-ref --verify --quiet "refs/heads/$TARGET_BRANCH"; then
        return 1
    fi

    # Check if metadata.json exists on target
    if ! git show "$TARGET_BRANCH:$checkpoint_path/metadata.json" &>/dev/null; then
        return 1
    fi

    # Check if same source commit (up-to-date)
    local target_source_commit
    target_source_commit=$(git show "$TARGET_BRANCH:$checkpoint_path/metadata.json" 2>/dev/null | jq -r '.migration_source_commit // ""')

    if [[ -n "$target_source_commit" && "$target_source_commit" == "$source_commit" ]]; then
        return 0
    fi

    return 1
}

# Migrate a single checkpoint directory
# Args: $1 = checkpoint path (e.g., "a1/b2c3d4e5f6"), $2 = source dir, $3 = target dir, $4 = source commit
# Returns: 0 if migrated, 1 if skipped
migrate_checkpoint() {
    local CHECKPOINT_DIR="$1"
    local SOURCE_DIR="$2"
    local TARGET_DIR="$3"
    local SOURCE_COMMIT="$4"
    local CHECKPOINT_PATH="$SOURCE_DIR/$CHECKPOINT_DIR"

    if [[ ! -f "$CHECKPOINT_PATH/metadata.json" ]]; then
        echo "    Skipping: no metadata.json"
        return 1
    fi

    local ROOT_META="$CHECKPOINT_PATH/metadata.json"

    # Check if this is session metadata (has session_id) or already aggregated
    if jq -e '.session_id' "$ROOT_META" > /dev/null 2>&1; then
        # This is session metadata at root - needs migration
        migrate_old_format "$CHECKPOINT_DIR" "$CHECKPOINT_PATH" "$TARGET_DIR" "$SOURCE_COMMIT"
    else
        # Already aggregated format - copy but still transform session metadata
        migrate_new_format "$CHECKPOINT_DIR" "$CHECKPOINT_PATH" "$TARGET_DIR" "$SOURCE_COMMIT"
    fi
    return 0
}

# Migrate checkpoint from old format (session files at root)
migrate_old_format() {
    local CHECKPOINT_DIR="$1"
    local CHECKPOINT_PATH="$2"
    local TARGET_DIR="$3"
    local SOURCE_COMMIT="$4"
    local ROOT_META="$CHECKPOINT_PATH/metadata.json"

    # Find existing numbered subdirs
    local EXISTING_SUBDIRS
    EXISTING_SUBDIRS=$(find "$CHECKPOINT_PATH" -maxdepth 1 -mindepth 1 -type d -name '[0-9]*' | sort -t'/' -k3 -n -r || true)

    # Calculate next session number (renumber existing + 1 for root)
    local NEXT_NUM=0

    # Renumber existing subdirs (in reverse to avoid conflicts)
    for SUBDIR in $EXISTING_SUBDIRS; do
        local OLD_NUM
        OLD_NUM=$(basename "$SUBDIR")
        local NEW_NUM=$((OLD_NUM + 1))

        # Copy to target with new number
        mkdir -p "$TARGET_DIR/$CHECKPOINT_DIR/$NEW_NUM"
        # Copy non-metadata files
        for FILE in context.md prompt.txt content_hash.txt full.jsonl; do
            if [[ -f "$SUBDIR/$FILE" ]]; then
                cp "$SUBDIR/$FILE" "$TARGET_DIR/$CHECKPOINT_DIR/$NEW_NUM/"
            fi
        done
        # Transform metadata.json: remove session_ids and session_count, convert agents array to single agent
        if [[ -f "$SUBDIR/metadata.json" ]]; then
            jq 'del(.session_ids, .session_count) | if .agents | type == "array" then .agents = .agents[0] else . end' \
                "$SUBDIR/metadata.json" > "$TARGET_DIR/$CHECKPOINT_DIR/$NEW_NUM/metadata.json"
        fi

        if [[ $NEW_NUM -gt $NEXT_NUM ]]; then
            NEXT_NUM=$NEW_NUM
        fi
    done

    # Move root session files to /0
    mkdir -p "$TARGET_DIR/$CHECKPOINT_DIR/0"
    # Copy non-metadata files
    for FILE in context.md prompt.txt content_hash.txt full.jsonl; do
        if [[ -f "$CHECKPOINT_PATH/$FILE" ]]; then
            cp "$CHECKPOINT_PATH/$FILE" "$TARGET_DIR/$CHECKPOINT_DIR/0/"
        fi
    done
    # Transform metadata.json
    if [[ -f "$CHECKPOINT_PATH/metadata.json" ]]; then
        jq 'del(.session_ids, .session_count) | if .agents | type == "array" then .agents = .agents[0] else . end' \
            "$CHECKPOINT_PATH/metadata.json" > "$TARGET_DIR/$CHECKPOINT_DIR/0/metadata.json"
    fi

    # Calculate total sessions (NEXT_NUM is highest 0-based index, so count = NEXT_NUM + 1)
    local TOTAL_SESSIONS=$((NEXT_NUM + 1))

    # Build sessions array and aggregate data
    local SESSIONS_JSON="[]"
    local FILES_TOUCHED="[]"
    local CHECKPOINTS_COUNT=0
    local INPUT_TOKENS=0
    local CACHE_CREATION=0
    local CACHE_READ=0
    local OUTPUT_TOKENS=0
    local API_CALLS=0

    for i in $(seq 0 $((TOTAL_SESSIONS - 1))); do
        local SESSION_DIR="$TARGET_DIR/$CHECKPOINT_DIR/$i"
        if [[ -d "$SESSION_DIR" ]]; then
            local SESSION_META="$SESSION_DIR/metadata.json"

            # Build session entry (paths are absolute from branch root)
            local SESSION_ENTRY
            SESSION_ENTRY=$(jq -n \
                --arg meta "/$CHECKPOINT_DIR/$i/metadata.json" \
                --arg transcript "/$CHECKPOINT_DIR/$i/full.jsonl" \
                --arg context "/$CHECKPOINT_DIR/$i/context.md" \
                --arg hash "/$CHECKPOINT_DIR/$i/content_hash.txt" \
                --arg prompt "/$CHECKPOINT_DIR/$i/prompt.txt" \
                '{metadata: $meta, transcript: $transcript, context: $context, content_hash: $hash, prompt: $prompt}')

            SESSIONS_JSON=$(echo "$SESSIONS_JSON" | jq --argjson entry "$SESSION_ENTRY" '. + [$entry]')

            # Aggregate from session metadata
            if [[ -f "$SESSION_META" ]]; then
                # Files touched (union)
                local SESSION_FILES
                SESSION_FILES=$(jq -r '.files_touched // []' "$SESSION_META")
                FILES_TOUCHED=$(echo "$FILES_TOUCHED" "$SESSION_FILES" | jq -s 'add | unique')

                # Checkpoints count (sum)
                CHECKPOINTS_COUNT=$((CHECKPOINTS_COUNT + $(jq -r '.checkpoints_count // 0' "$SESSION_META")))

                # Token usage (sum)
                INPUT_TOKENS=$((INPUT_TOKENS + $(jq -r '.token_usage.input_tokens // 0' "$SESSION_META")))
                CACHE_CREATION=$((CACHE_CREATION + $(jq -r '.token_usage.cache_creation_tokens // 0' "$SESSION_META")))
                CACHE_READ=$((CACHE_READ + $(jq -r '.token_usage.cache_read_tokens // 0' "$SESSION_META")))
                OUTPUT_TOKENS=$((OUTPUT_TOKENS + $(jq -r '.token_usage.output_tokens // 0' "$SESSION_META")))
                API_CALLS=$((API_CALLS + $(jq -r '.token_usage.api_call_count // 0' "$SESSION_META")))
            fi
        fi
    done

    # Get base info from original root metadata
    local CHECKPOINT_ID STRATEGY BRANCH
    CHECKPOINT_ID=$(jq -r '.checkpoint_id // ""' "$ROOT_META")
    STRATEGY=$(jq -r '.strategy // "manual-commit"' "$ROOT_META")
    BRANCH=$(jq -r '.branch // ""' "$ROOT_META")

    # Create aggregated metadata.json
    jq -n \
        --arg checkpoint_id "$CHECKPOINT_ID" \
        --arg strategy "$STRATEGY" \
        --arg branch "$BRANCH" \
        --arg migration_source_commit "$SOURCE_COMMIT" \
        --argjson checkpoints_count "$CHECKPOINTS_COUNT" \
        --argjson files_touched "$FILES_TOUCHED" \
        --argjson sessions "$SESSIONS_JSON" \
        --argjson input_tokens "$INPUT_TOKENS" \
        --argjson cache_creation "$CACHE_CREATION" \
        --argjson cache_read "$CACHE_READ" \
        --argjson output_tokens "$OUTPUT_TOKENS" \
        --argjson api_calls "$API_CALLS" \
        '{
            checkpoint_id: $checkpoint_id,
            strategy: $strategy,
            branch: $branch,
            migration_source_commit: $migration_source_commit,
            checkpoints_count: $checkpoints_count,
            files_touched: $files_touched,
            sessions: $sessions,
            token_usage: {
                input_tokens: $input_tokens,
                cache_creation_tokens: $cache_creation,
                cache_read_tokens: $cache_read,
                output_tokens: $output_tokens,
                api_call_count: $api_calls
            }
        }' > "$TARGET_DIR/$CHECKPOINT_DIR/metadata.json"

    echo "    Migrated: $TOTAL_SESSIONS session(s)"
}

# Migrate checkpoint that's already in new format (just transform paths)
migrate_new_format() {
    local CHECKPOINT_DIR="$1"
    local CHECKPOINT_PATH="$2"
    local TARGET_DIR="$3"
    local SOURCE_COMMIT="$4"

    mkdir -p "$TARGET_DIR/$CHECKPOINT_DIR"

    # Transform root metadata.json to have absolute paths in sessions array and add source commit
    jq --arg prefix "/$CHECKPOINT_DIR" --arg source_commit "$SOURCE_COMMIT" \
        '.migration_source_commit = $source_commit | .sessions = [.sessions[] | {
            metadata: ($prefix + "/" + (.metadata | ltrimstr("/"))),
            transcript: ($prefix + "/" + (.transcript | ltrimstr("/"))),
            context: ($prefix + "/" + (.context | ltrimstr("/"))),
            content_hash: ($prefix + "/" + (.content_hash | ltrimstr("/"))),
            prompt: ($prefix + "/" + (.prompt | ltrimstr("/")))
        }]' "$CHECKPOINT_PATH/metadata.json" > "$TARGET_DIR/$CHECKPOINT_DIR/metadata.json"

    # Copy and transform each session subdir's metadata.json
    for SUBDIR in $(find "$CHECKPOINT_PATH" -maxdepth 1 -mindepth 1 -type d -name '[0-9]*'); do
        local SUBDIR_NUM
        SUBDIR_NUM=$(basename "$SUBDIR")
        mkdir -p "$TARGET_DIR/$CHECKPOINT_DIR/$SUBDIR_NUM"

        # Copy non-metadata files
        for FILE in context.md prompt.txt content_hash.txt full.jsonl; do
            if [[ -f "$SUBDIR/$FILE" ]]; then
                cp "$SUBDIR/$FILE" "$TARGET_DIR/$CHECKPOINT_DIR/$SUBDIR_NUM/"
            fi
        done

        # Transform metadata.json
        if [[ -f "$SUBDIR/metadata.json" ]]; then
            jq 'del(.session_ids, .session_count) | if .agents | type == "array" then .agents = .agents[0] else . end' \
                "$SUBDIR/metadata.json" > "$TARGET_DIR/$CHECKPOINT_DIR/$SUBDIR_NUM/metadata.json"
        fi
    done
    echo "    Copied with session metadata transformed"
}

# Single checkpoint migration mode
if [[ -n "$CHECKPOINT_FILTER" ]]; then
    CHECKPOINT_PATH=$(checkpoint_to_path "$CHECKPOINT_FILTER")
    echo -e "${GREEN}Migrating single checkpoint: $CHECKPOINT_FILTER${NC}"
    echo "  Path: $CHECKPOINT_PATH"

    # Find the most recent commit that modified this checkpoint
    SOURCE_COMMIT=$(git log -1 --format="%H" "$SOURCE_BRANCH" -- "$CHECKPOINT_PATH")
    if [[ -z "$SOURCE_COMMIT" ]]; then
        echo -e "${RED}Error: Checkpoint $CHECKPOINT_FILTER not found on $SOURCE_BRANCH${NC}" >&2
        exit 1
    fi
    COMMIT_AUTHOR=$(git log -1 --format="%an <%ae>" "$SOURCE_COMMIT")
    echo "  Source commit: ${SOURCE_COMMIT:0:7} (by $COMMIT_AUTHOR)"

    # Create temp dir and checkout source
    TEMP_DIR=$(mktemp -d)
    git worktree add --detach "$TEMP_DIR" "$SOURCE_COMMIT" 2>/dev/null

    if [[ ! -d "$TEMP_DIR/$CHECKPOINT_PATH" ]]; then
        git worktree remove "$TEMP_DIR" --force 2>/dev/null || rm -rf "$TEMP_DIR"
        echo -e "${RED}Error: Checkpoint $CHECKPOINT_FILTER not found on $SOURCE_BRANCH${NC}" >&2
        exit 1
    fi

    # Check if already up-to-date
    if checkpoint_up_to_date_on_target "$CHECKPOINT_PATH" "$SOURCE_COMMIT"; then
        git worktree remove "$TEMP_DIR" --force 2>/dev/null || rm -rf "$TEMP_DIR"
        echo -e "  ${YELLOW}Already up-to-date on $TARGET_BRANCH - skipping${NC}"
        exit 0
    fi

    # Show checkpoint info
    if jq -e '.session_id' "$TEMP_DIR/$CHECKPOINT_PATH/metadata.json" > /dev/null 2>&1; then
        echo "  Format: old (session files at root) -> needs migration"
        SESSION_COUNT=$(find "$TEMP_DIR/$CHECKPOINT_PATH" -maxdepth 1 -mindepth 1 -type d -name '[0-9]*' | wc -l | tr -d ' ')
        echo "  Sessions: $((SESSION_COUNT + 1)) (1 at root + $SESSION_COUNT archived)"
    else
        echo "  Format: new (already has CheckpointSummary)"
    fi

    if [[ "$DRY_RUN" == "true" ]]; then
        git worktree remove "$TEMP_DIR" --force 2>/dev/null || rm -rf "$TEMP_DIR"
        echo ""
        echo -e "${YELLOW}Run with --apply to perform migration${NC}"
        exit 0
    fi

    # Ensure target branch exists
    if ! git show-ref --verify --quiet "refs/heads/$TARGET_BRANCH"; then
        echo -e "${GREEN}Creating target branch $TARGET_BRANCH...${NC}"
        git checkout "$SOURCE_BRANCH"
        git checkout --orphan "$TARGET_BRANCH"
        git commit --allow-empty -m "Initialize metadata branch (v1)"
        git checkout "$SOURCE_BRANCH"
    fi

    # Checkout target branch
    git checkout "$TARGET_BRANCH"

    # Migrate the checkpoint
    migrate_checkpoint "$CHECKPOINT_PATH" "$TEMP_DIR" "$(pwd)" "$SOURCE_COMMIT"

    # Cleanup
    git worktree remove "$TEMP_DIR" --force 2>/dev/null || rm -rf "$TEMP_DIR"

    # Commit with original author (with retry for lock issues)
    git_retry git add "$CHECKPOINT_PATH"
    if ! git diff --cached --quiet; then
        git_retry git commit --author="$COMMIT_AUTHOR" -m "Migrate checkpoint: $CHECKPOINT_FILTER"
        echo -e "${GREEN}Committed (author: $COMMIT_AUTHOR)${NC}"
    else
        echo -e "${YELLOW}No changes${NC}"
    fi

    git checkout "$ORIGINAL_BRANCH" 2>/dev/null || git checkout main
    echo ""
    echo -e "${GREEN}=== Migration Complete ===${NC}"
    exit 0
fi

# Full migration mode - process all commits
# Get list of commits from source branch (oldest first, excluding initial commit)
COMMITS=$(git log --reverse --format="%H" "$SOURCE_BRANCH" | tail -n +2)
INIT_COMMIT=$(git log --reverse --format="%H" "$SOURCE_BRANCH" | head -1)

COMMIT_COUNT=$(echo "$COMMITS" | wc -l | tr -d ' ')
echo -e "${YELLOW}Found $COMMIT_COUNT commits to process:${NC}"
git log --reverse --oneline "$SOURCE_BRANCH" | tail -n +2
echo ""

if [[ "$DRY_RUN" == "true" ]]; then
    # In dry-run, show what checkpoints exist
    TEMP_DIR=$(mktemp -d)
    git worktree add --detach "$TEMP_DIR" "$SOURCE_BRANCH" 2>/dev/null

    cd "$TEMP_DIR"
    CHECKPOINT_DIRS=$(find . -maxdepth 2 -mindepth 2 -type d | grep -E '^\./[0-9a-f]{2}/[0-9a-f]+$' || true)
    CHECKPOINT_COUNT=$(echo "$CHECKPOINT_DIRS" | grep -c . || echo 0)

    echo -e "${YELLOW}Found $CHECKPOINT_COUNT checkpoints on $SOURCE_BRANCH:${NC}"
    for CHECKPOINT_PATH in $CHECKPOINT_DIRS; do
        CHECKPOINT_DIR="${CHECKPOINT_PATH#./}"
        if [[ -f "$CHECKPOINT_PATH/metadata.json" ]]; then
            # Get the source commit for this checkpoint
            SOURCE_COMMIT=$(git log -1 --format="%H" "$SOURCE_BRANCH" -- "$CHECKPOINT_DIR")
            if checkpoint_up_to_date_on_target "$CHECKPOINT_DIR" "$SOURCE_COMMIT"; then
                echo -e "  $CHECKPOINT_DIR ${GREEN}(up-to-date)${NC}"
            elif jq -e '.session_id' "$CHECKPOINT_PATH/metadata.json" > /dev/null 2>&1; then
                echo "  $CHECKPOINT_DIR (old format -> will migrate)"
            else
                echo "  $CHECKPOINT_DIR (new format -> will migrate)"
            fi
        fi
    done

    cd "$OLDPWD"
    git worktree remove "$TEMP_DIR" --force 2>/dev/null || rm -rf "$TEMP_DIR"

    echo ""
    echo -e "${YELLOW}Run with --apply to perform migration${NC}"
    exit 0
fi

# Create orphan target branch if it doesn't exist
if git show-ref --verify --quiet "refs/heads/$TARGET_BRANCH"; then
    echo -e "${YELLOW}Target branch $TARGET_BRANCH already exists - will skip existing checkpoints${NC}"
else
    echo -e "${GREEN}Creating target branch $TARGET_BRANCH...${NC}"
    git checkout "$SOURCE_BRANCH"
    git checkout "$INIT_COMMIT"
    git checkout --orphan "$TARGET_BRANCH"
    git commit --allow-empty -m "Initialize metadata branch (v1)"
    git checkout "$SOURCE_BRANCH"
fi

# Process each commit
for COMMIT in $COMMITS; do
    COMMIT_MSG=$(git log -1 --format="%s" "$COMMIT")

    # Find checkpoint directories MODIFIED in this commit (not all checkpoints in tree)
    CHECKPOINT_DIRS=$(git diff-tree --no-commit-id --name-only -r "$COMMIT" | grep -E '^[0-9a-f]{2}/[0-9a-f]+/' | cut -d'/' -f1-2 | sort -u || true)

    if [[ -z "$CHECKPOINT_DIRS" ]]; then
        echo -e "${YELLOW}Skipping commit (no checkpoint changes): $COMMIT_MSG${NC}"
        continue
    fi

    # Check if any of the modified checkpoints need migration
    NEEDS_MIGRATION=false
    for CHECKPOINT_DIR in $CHECKPOINT_DIRS; do
        if ! checkpoint_up_to_date_on_target "$CHECKPOINT_DIR" "$COMMIT"; then
            NEEDS_MIGRATION=true
            break
        fi
    done

    if [[ "$NEEDS_MIGRATION" == "false" ]]; then
        echo -e "${YELLOW}Skipping commit (all up-to-date): $COMMIT_MSG${NC}"
        continue
    fi

    echo -e "${GREEN}Processing commit: $COMMIT_MSG${NC}"

    # Get original author for preserving authorship
    COMMIT_AUTHOR=$(git log -1 --format="%an <%ae>" "$COMMIT")

    # Checkout source commit in temp worktree
    TEMP_DIR=$(mktemp -d)
    git worktree add --detach "$TEMP_DIR" "$COMMIT" 2>/dev/null

    # Checkout target branch
    git checkout "$TARGET_BRANCH"

    # Track which checkpoint directories we process
    PROCESSED_DIRS=""

    # Process checkpoints
    for CHECKPOINT_DIR in $CHECKPOINT_DIRS; do
        # Skip if already up-to-date on target
        if checkpoint_up_to_date_on_target "$CHECKPOINT_DIR" "$COMMIT"; then
            echo "  Skipping checkpoint (up-to-date): $CHECKPOINT_DIR"
            continue
        fi

        echo "  Processing checkpoint: $CHECKPOINT_DIR"

        if migrate_checkpoint "$CHECKPOINT_DIR" "$TEMP_DIR" "$(pwd)" "$COMMIT"; then
            # Track this directory for git add later
            PROCESSED_DIRS="$PROCESSED_DIRS $CHECKPOINT_DIR"
        fi
    done

    # Cleanup worktree
    git worktree remove "$TEMP_DIR" --force 2>/dev/null || rm -rf "$TEMP_DIR"

    # Only add the specific checkpoint directories we processed (with retry for lock issues)
    for DIR in $PROCESSED_DIRS; do
        git_retry git add "$DIR"
    done

    # Commit changes with original author (with retry for lock issues)
    if ! git diff --cached --quiet; then
        git_retry git commit --author="$COMMIT_AUTHOR" -m "$COMMIT_MSG"
        echo -e "  ${GREEN}Committed (author: $COMMIT_AUTHOR)${NC}"
    else
        echo -e "  ${YELLOW}No changes${NC}"
    fi
done

# Return to original branch
git checkout "$ORIGINAL_BRANCH" 2>/dev/null || git checkout main

echo ""
echo -e "${GREEN}=== Migration Complete ===${NC}"
echo "New branch: $TARGET_BRANCH"
echo ""
echo "To verify:"
echo "  git log $TARGET_BRANCH"
echo "  git show $TARGET_BRANCH:<checkpoint_path>/metadata.json"
