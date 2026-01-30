#!/bin/bash
# End-to-end test for attribution tracking with an ABANDONED SESSION
# Tests the scenario: Session 1 modifies files -> git restore (discard) -> Session 2 modifies files -> Commit
# The attribution should ONLY include Session 2 since Session 1's changes were discarded.
# Usage: ./scripts/test-attribution-e2e-abandoned-session.sh [--keep]
#   --keep: Don't delete the test repo after running (for inspection)

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Store the CLI directory and build the binary fresh
CLI_DIR="$(cd "$(dirname "$0")/.." && pwd)"
echo -e "${BLUE}Building entire CLI from: $CLI_DIR${NC}"

# Build binary to a temp directory and add it to PATH
# This ensures BOTH our direct calls AND Claude's hook calls use the new binary
ENTIRE_BIN_DIR=$(mktemp -d)
ENTIRE_BIN="$ENTIRE_BIN_DIR/entire"
if ! go build -o "$ENTIRE_BIN" "$CLI_DIR/cmd/entire"; then
    echo -e "${RED}Failed to build entire CLI${NC}"
    exit 1
fi
chmod +x "$ENTIRE_BIN"
echo -e "${GREEN}Built: $ENTIRE_BIN${NC}"

# Add the binary directory to PATH so Claude's hooks find it
export PATH="$ENTIRE_BIN_DIR:$PATH"
echo -e "${GREEN}Added to PATH: $ENTIRE_BIN_DIR${NC}"

# Verify the right binary is being used
echo -e "${BLUE}Verifying entire location:${NC} $(which entire)"

KEEP_REPO=false
if [[ "$1" == "--keep" ]]; then
    KEEP_REPO=true
fi

# Create temp directory for test repo
TEST_DIR=$(mktemp -d)
echo -e "${BLUE}=== Creating test repo in: $TEST_DIR ===${NC}"

cleanup() {
    # Always clean up the temp binary directory
    rm -rf "$ENTIRE_BIN_DIR"

    if [[ "$KEEP_REPO" == "true" ]]; then
        echo -e "${YELLOW}Keeping test repo at: $TEST_DIR${NC}"
    else
        echo -e "${BLUE}Cleaning up test repo...${NC}"
        rm -rf "$TEST_DIR"
    fi
}
trap cleanup EXIT

cd "$TEST_DIR"

# Initialize git repo
echo -e "${BLUE}=== Step 1: Initialize git repo ===${NC}"
git init
git config user.email "test@example.com"
git config user.name "Test User"

# Create initial file and commit
echo -e "${BLUE}=== Step 2: Create initial commit ===${NC}"
cat > main.py << 'EOF'
#!/usr/bin/env python3
"""Main entry point."""

def main():
    print("Hello, World!")

if __name__ == "__main__":
    main()
EOF
git add main.py
git commit -m "Initial commit"

# Enable entire
echo -e "${BLUE}=== Step 3: Enable entire ===${NC}"
entire enable --strategy manual-commit

# Commit the setup files to establish a clean baseline
echo -e "${BLUE}=== Step 3b: Commit setup files (clean baseline) ===${NC}"
git add .claude/ .entire/
git commit -m "Setup entire tracking"
echo -e "${GREEN}Baseline established - .claude/ and .entire/ are now committed${NC}"

# Capture the HEAD commit hash for shadow branch verification
BASE_COMMIT=$(git rev-parse HEAD)
BASE_COMMIT_SHORT="${BASE_COMMIT:0:7}"
echo -e "${GREEN}Base commit: $BASE_COMMIT_SHORT${NC}"

# Run first Claude prompt - SESSION 1 adds a function
echo -e "${BLUE}=== Step 4: SESSION 1 - Add password hashing function ===${NC}"
echo "Session 1: Adding password hashing function via Claude..."
claude --model haiku -p "Add a function called hash_password(password) to main.py that returns a hashed version of the password using hashlib.sha256. Import hashlib at the top. Don't modify anything else." --allowedTools Edit Read

# Show what changed
echo -e "${GREEN}Files after Session 1:${NC}"
cat main.py
echo ""

# Show git status after first session
echo -e "${BLUE}=== Step 5: Git status after Session 1 ===${NC}"
git status --short
echo ""

# Verify shadow branch exists (pattern: entire/<commit>-<session-num>)
echo -e "${BLUE}=== Step 6: Verify shadow branch exists ===${NC}"
SHADOW_BRANCH=$(git branch --list "entire/${BASE_COMMIT_SHORT}-*" | head -1 | tr -d ' *')
if [[ -n "$SHADOW_BRANCH" ]]; then
    echo -e "${GREEN}Shadow branch exists: ${SHADOW_BRANCH}${NC}"
    echo "Shadow branch commits:"
    git log --oneline "${SHADOW_BRANCH}" | head -5
else
    echo -e "${RED}ERROR: No shadow branch matching entire/${BASE_COMMIT_SHORT}-* found!${NC}"
    echo "Available branches:"
    git branch -a
    exit 1
fi

# Check rewind points after Session 1
echo -e "${BLUE}=== Step 7: Rewind points after Session 1 ===${NC}"
entire rewind --list || true
echo ""

# Capture Session 1 ID for later verification
GIT_DIR=$(git rev-parse --git-dir)
SESSION1_ID=""
if [[ -d "$GIT_DIR/entire-sessions" ]]; then
    for f in "$GIT_DIR/entire-sessions"/*.json; do
        if [[ -f "$f" ]]; then
            SESSION1_ID=$(basename "$f" .json)
            echo -e "${GREEN}Session 1 ID: $SESSION1_ID${NC}"
            break
        fi
    done
fi

# NOW DISCARD ALL CHANGES - simulating user abandoning their work
echo -e "${BLUE}=== Step 8: ABANDON SESSION 1 - git restore (discard all changes) ===${NC}"
echo "Discarding Session 1 changes with git restore..."
git restore main.py
echo -e "${YELLOW}All Session 1 changes discarded!${NC}"
echo ""

# Verify file is back to original
echo -e "${GREEN}main.py after git restore:${NC}"
cat main.py
echo ""

# Verify working tree is clean
echo -e "${BLUE}=== Step 9: Verify working tree is clean ===${NC}"
git status --short
if [[ -z "$(git status --porcelain)" ]]; then
    echo -e "${GREEN}Working tree is clean - Session 1 changes successfully discarded${NC}"
else
    echo -e "${YELLOW}Working tree has changes (unexpected)${NC}"
fi
echo ""

# Now start SESSION 2 - this is a NEW session with DIFFERENT changes
echo -e "${BLUE}=== Step 10: SESSION 2 - Add random number function ===${NC}"
echo "Session 2: Adding random number function to main.py..."
claude --model haiku -p "Add a function called get_random_number() to main.py that returns a random integer between 1 and 100. Import random at the top. Don't modify anything else." --allowedTools Edit Read

# Show what changed
echo -e "${GREEN}main.py after Session 2:${NC}"
cat main.py
echo ""

# Verify Session 1's code is NOT present
echo -e "${BLUE}=== Step 11: Verify Session 1 code is NOT in file ===${NC}"
if grep -q "hash_password\|hashlib" main.py; then
    echo -e "${RED}ERROR: Session 1 code (hash_password/hashlib) found in main.py!${NC}"
    echo "This should not happen - Session 1 was abandoned."
    exit 1
else
    echo -e "${GREEN}Confirmed: Session 1 code (hash_password/hashlib) is NOT in file${NC}"
fi

# Verify Session 2's code IS present
if grep -q "get_random_number\|random" main.py; then
    echo -e "${GREEN}Confirmed: Session 2 code (get_random_number/random) IS in file${NC}"
else
    echo -e "${RED}ERROR: Session 2 code not found in main.py!${NC}"
    exit 1
fi
echo ""

# Show git status after second session
echo -e "${BLUE}=== Step 12: Git status after Session 2 ===${NC}"
git status --short
echo ""

# Check rewind points - may show checkpoints from both sessions
echo -e "${BLUE}=== Step 13: Rewind points after Session 2 ===${NC}"
entire rewind --list || true
echo ""

# Check shadow branches before commit
echo -e "${BLUE}=== Step 13b: Shadow branches before commit ===${NC}"
echo "Branches:"
git branch -a
echo ""
echo "Session state files:"
for f in .git/entire-sessions/*.json; do
    if [[ -f "$f" ]]; then
        echo "--- $f ---"
        jq '{session_id, shadow_branch_suffix, files_touched}' "$f"
    fi
done

# List files on each shadow branch
for branch in $(git branch --list "entire/${BASE_COMMIT_SHORT}-*" | tr -d ' *'); do
    echo ""
    echo "=== Files on $branch ==="
    echo "main.py content:"
    git show "${branch}:main.py" 2>/dev/null | head -20 || echo "(no main.py)"
done
echo ""

# Now commit and check attribution
echo -e "${BLUE}=== Step 14: Stage and commit ===${NC}"
git add -A
git commit -m "Add random number utility"

# Show the commit with trailers
echo -e "${GREEN}Commit details:${NC}"
git log -1 --format=full

# Check for Entire-Checkpoint trailer
echo ""
echo -e "${BLUE}=== Step 15: Check attribution in commit ===${NC}"
CHECKPOINT_ID=$(git log -1 --format=%B | grep "Entire-Checkpoint:" | cut -d: -f2 | tr -d ' ')
if [[ -n "$CHECKPOINT_ID" ]]; then
    echo -e "${GREEN}Found Entire-Checkpoint: $CHECKPOINT_ID${NC}"

    # Extract the sharded path: first 2 chars / remaining chars
    SHARD_PREFIX="${CHECKPOINT_ID:0:2}"
    SHARD_SUFFIX="${CHECKPOINT_ID:2}"
    METADATA_PATH="${SHARD_PREFIX}/${SHARD_SUFFIX}/metadata.json"

    echo ""
    echo -e "${BLUE}=== Step 16: Inspect metadata on entire/sessions branch ===${NC}"
    echo "Looking for metadata at: $METADATA_PATH"

    # Read metadata.json from entire/sessions branch
    if git show "entire/sessions:${METADATA_PATH}" > /dev/null 2>&1; then
        echo -e "${GREEN}Found metadata.json:${NC}"
        git show "entire/sessions:${METADATA_PATH}" | jq .

        # Check session_ids - should only have Session 2
        echo ""
        echo -e "${BLUE}=== Step 17: Session validation ===${NC}"
        SESSION_COUNT=$(git show "entire/sessions:${METADATA_PATH}" | jq -r '.session_count // 1')
        SESSION_IDS=$(git show "entire/sessions:${METADATA_PATH}" | jq -r '.session_ids // []')
        MAIN_SESSION=$(git show "entire/sessions:${METADATA_PATH}" | jq -r '.session_id')
        echo "Session count: $SESSION_COUNT"
        echo "Session IDs: $SESSION_IDS"
        echo "Main session: $MAIN_SESSION"

        if [[ "$SESSION_COUNT" -eq 1 ]]; then
            echo -e "${GREEN}Only one session in metadata - expected for abandoned session scenario${NC}"
        else
            echo -e "${YELLOW}Multiple sessions detected - Session 1 may have been included${NC}"
        fi

        # Verify Session 1 is NOT in the metadata
        if [[ -n "$SESSION1_ID" ]] && echo "$SESSION_IDS" | grep -q "$SESSION1_ID"; then
            echo -e "${RED}ERROR: Session 1 ($SESSION1_ID) was included in metadata!${NC}"
            echo "Session 1 changes were discarded, so it should NOT be attributed."
        else
            echo -e "${GREEN}Confirmed: Session 1 ($SESSION1_ID) is NOT in metadata${NC}"
        fi

        # Extract and display attribution specifically
        echo ""
        echo -e "${BLUE}=== Step 18: Attribution Analysis ===${NC}"
        ATTRIBUTION=$(git show "entire/sessions:${METADATA_PATH}" | jq -r '.initial_attribution // empty')
        if [[ -n "$ATTRIBUTION" && "$ATTRIBUTION" != "null" ]]; then
            echo -e "${GREEN}Attribution data:${NC}"
            echo "$ATTRIBUTION" | jq .

            # Extract key values
            AGENT_LINES=$(echo "$ATTRIBUTION" | jq -r '.agent_lines')
            HUMAN_ADDED=$(echo "$ATTRIBUTION" | jq -r '.human_added')
            TOTAL=$(echo "$ATTRIBUTION" | jq -r '.total_committed')
            PERCENTAGE=$(echo "$ATTRIBUTION" | jq -r '.agent_percentage')

            echo ""
            echo -e "${GREEN}Summary:${NC}"
            echo "  Agent lines:     $AGENT_LINES"
            echo "  Human added:     $HUMAN_ADDED"
            echo "  Total committed: $TOTAL"
            echo "  Agent %:         $PERCENTAGE"

            # Verify attribution only includes Session 2's changes
            # Session 2 added get_random_number which should be ~3-5 lines
            # Session 1's hash_password should NOT be counted
            echo ""
            echo -e "${BLUE}=== Step 19: Attribution Validation ===${NC}"

            # Check that the attributed lines are reasonable for just Session 2
            # (not doubled by including Session 1)
            if [[ "$AGENT_LINES" -gt 0 ]]; then
                echo -e "${GREEN}Agent attribution: $AGENT_LINES lines${NC}"
                echo "This should represent ONLY Session 2's get_random_number() function"
            else
                echo -e "${YELLOW}No agent lines attributed${NC}"
            fi
        else
            echo -e "${YELLOW}No initial_attribution in metadata${NC}"
        fi

        # Show files in checkpoint directory
        echo ""
        echo -e "${BLUE}=== Step 20: Checkpoint directory contents ===${NC}"
        echo "Files in checkpoint directory:"
        git ls-tree -r --name-only "entire/sessions" | grep "^${SHARD_PREFIX}/${SHARD_SUFFIX}/" | head -30

    else
        echo -e "${RED}Could not find metadata at $METADATA_PATH${NC}"
        echo "Checking what's on entire/sessions branch:"
        git ls-tree -r --name-only "entire/sessions" 2>/dev/null | head -20 || echo "(branch may not exist)"
    fi
else
    echo -e "${YELLOW}No Entire-Checkpoint trailer found (user may have removed it)${NC}"
fi

# Final summary
echo ""
echo -e "${GREEN}=== Test Complete ===${NC}"
echo "Test repo location: $TEST_DIR"
echo ""
echo "What was tested:"
echo "  1. Session 1: Agent added hash_password() to main.py"
echo "  2. User ABANDONED Session 1 via 'git restore' (discarded all changes)"
echo "  3. Session 2: Agent added get_random_number() to main.py"
echo "  4. Commit containing ONLY Session 2's work"
echo ""
echo "Expected behavior:"
echo "  - Shadow branch should exist after Session 1"
echo "  - After git restore, Session 1's code should be gone"
echo "  - Final commit should ONLY attribute Session 2"
echo "  - Session 1 should NOT appear in session_ids or attribution"
echo "  - hash_password/hashlib should NOT be in the committed code"
echo "  - get_random_number/random SHOULD be in the committed code"
echo ""
if [[ "$KEEP_REPO" == "true" ]]; then
    echo -e "${YELLOW}Repo kept for inspection. To clean up: rm -rf $TEST_DIR${NC}"
    echo ""
    echo "Useful inspection commands:"
    echo "  cd $TEST_DIR"
    echo "  git log entire/sessions --oneline"
    echo "  entire rewind --list"
    echo "  git show entire/sessions:<checkpoint-path>/metadata.json | jq ."
fi
