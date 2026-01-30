#!/bin/bash
# End-to-end test for attribution tracking with a SECOND SESSION on the same files
# Tests the scenario: Session 1 modifies files -> Session 2 modifies same files -> Commit
# Usage: ./scripts/test-attribution-e2e-second-session.sh [--keep]
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

# Run first Claude prompt - SESSION 1 adds a function
echo -e "${BLUE}=== Step 4: SESSION 1 - Add random number function ===${NC}"
echo "Session 1: Adding random number function via Claude..."
claude --model haiku -p "Add a function called get_random_number() to main.py that returns a random integer between 1 and 100. Import random at the top. Don't modify anything else." --allowedTools Edit Read

# Show what changed
echo -e "${GREEN}Files after Session 1:${NC}"
cat main.py
echo ""

# Show git status after first session
echo -e "${BLUE}=== Step 5: Git status after Session 1 ===${NC}"
git status --short
echo ""

# Check rewind points after Session 1
echo -e "${BLUE}=== Step 6: Rewind points after Session 1 ===${NC}"
entire rewind --list || true
echo ""

# Now start SESSION 2 - this is a NEW session working on the SAME files
echo -e "${BLUE}=== Step 7: SESSION 2 - Modify same file (add another function) ===${NC}"
echo "Session 2: Adding another function to main.py (same file as Session 1)..."
claude --model haiku -p "Add a function called get_random_string(length) to main.py that returns a random string of the given length using letters and digits. Import string module at the top. Put this function after get_random_number(). Don't modify existing functions." --allowedTools Edit Read

# Show what changed
echo -e "${GREEN}main.py after Session 2:${NC}"
cat main.py
echo ""

# Show git status after second session
echo -e "${BLUE}=== Step 8: Git status after Session 2 ===${NC}"
git status --short
echo ""

# Check rewind points - should show checkpoints from BOTH sessions
echo -e "${BLUE}=== Step 9: Rewind points (should show both sessions) ===${NC}"
entire rewind --list || true
echo ""

# Show session state files (should show multiple sessions)
echo -e "${BLUE}=== Step 10: Session state files ===${NC}"
GIT_DIR=$(git rev-parse --git-dir)
if [[ -d "$GIT_DIR/entire-sessions" ]]; then
    for f in "$GIT_DIR/entire-sessions"/*.json; do
        if [[ -f "$f" ]]; then
            echo -e "${GREEN}$f:${NC}"
            jq . "$f" 2>/dev/null || cat "$f"
            echo ""
        fi
    done
else
    echo "(no session state directory)"
fi

# User also makes a small edit
echo -e "${BLUE}=== Step 11: User adds a comment ===${NC}"
cat >> main.py << 'EOF'

# User added this version marker
VERSION = "1.0.0"
EOF
echo -e "${GREEN}main.py after user edit:${NC}"
tail -5 main.py
echo ""

# Now commit and check attribution
echo -e "${BLUE}=== Step 12: Stage and commit ===${NC}"
git add -A
git commit -m "Add random utilities from two sessions"

# Show the commit with trailers
echo -e "${GREEN}Commit details:${NC}"
git log -1 --format=full

# Check for Entire-Checkpoint trailer
echo ""
echo -e "${BLUE}=== Step 13: Check attribution in commit ===${NC}"
CHECKPOINT_ID=$(git log -1 --format=%B | grep "Entire-Checkpoint:" | cut -d: -f2 | tr -d ' ')
if [[ -n "$CHECKPOINT_ID" ]]; then
    echo -e "${GREEN}Found Entire-Checkpoint: $CHECKPOINT_ID${NC}"

    # Extract the sharded path: first 2 chars / remaining chars
    SHARD_PREFIX="${CHECKPOINT_ID:0:2}"
    SHARD_SUFFIX="${CHECKPOINT_ID:2}"
    METADATA_PATH="${SHARD_PREFIX}/${SHARD_SUFFIX}/metadata.json"

    echo ""
    echo -e "${BLUE}=== Step 14: Inspect metadata on entire/sessions branch ===${NC}"
    echo "Looking for metadata at: $METADATA_PATH"

    # Read metadata.json from entire/sessions branch
    if git show "entire/sessions:${METADATA_PATH}" > /dev/null 2>&1; then
        echo -e "${GREEN}Found metadata.json:${NC}"
        git show "entire/sessions:${METADATA_PATH}" | jq .

        # Check session_ids - should have multiple sessions
        echo ""
        echo -e "${BLUE}=== Step 15: Multi-session check ===${NC}"
        SESSION_COUNT=$(git show "entire/sessions:${METADATA_PATH}" | jq -r '.session_count // 1')
        SESSION_IDS=$(git show "entire/sessions:${METADATA_PATH}" | jq -r '.session_ids // []')
        echo "Session count: $SESSION_COUNT"
        echo "Session IDs: $SESSION_IDS"

        if [[ "$SESSION_COUNT" -gt 1 ]]; then
            echo -e "${GREEN}Multiple sessions detected - test scenario working!${NC}"
        else
            echo -e "${YELLOW}Only one session detected (sessions may have merged or only one ran)${NC}"
        fi

        # Extract and display attribution specifically
        echo ""
        echo -e "${BLUE}=== Step 16: Attribution Analysis ===${NC}"
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
        else
            echo -e "${YELLOW}No initial_attribution in metadata${NC}"
        fi

        # Show files in checkpoint directory (should include archived sessions)
        echo ""
        echo -e "${BLUE}=== Step 17: Checkpoint directory contents ===${NC}"
        echo "Files in checkpoint directory (look for numbered subdirs like 1/, 2/ for archived sessions):"
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
echo "  1. Session 1: Agent added get_random_number() to main.py"
echo "  2. Session 2: Agent added get_random_string() to SAME main.py"
echo "  3. User added VERSION constant"
echo "  4. Single commit containing work from BOTH sessions"
echo "  5. Attribution should track lines from both agent sessions"
echo ""
echo "Expected behavior:"
echo "  - Both sessions should have checkpoints on the shadow branch"
echo "  - Metadata should show session_count > 1 (or session_ids array)"
echo "  - Attribution should include agent lines from both sessions"
echo "  - Checkpoint directory may have archived session subdirs (1/, 2/)"
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
