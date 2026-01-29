#!/bin/bash
# End-to-end test for attribution tracking with real Claude calls
# Usage: ./scripts/test-attribution-e2e.sh [--keep]
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

# Run first Claude prompt - have agent add a function
echo -e "${BLUE}=== Step 4: Run first Claude prompt (agent adds random number function) ===${NC}"
echo "Adding random number function via Claude..."
claude --model haiku -p "Add a function called get_random_number() to main.py that returns a random integer between 1 and 100. Import random at the top. Don't modify anything else." --allowedTools Edit Read

# Show what changed
echo -e "${GREEN}Files after first prompt:${NC}"
cat main.py
echo ""

# Show git status after first prompt
echo -e "${BLUE}=== Step 5: Git status after first prompt ===${NC}"
git status --short

# User manually adds a new file (simulating user edits between prompts)
echo -e "${BLUE}=== Step 6: User adds a new file (utils.py) ===${NC}"
cat > utils.py << 'EOF'
"""Utility functions."""

def format_number(n):
    """Format a number with commas."""
    return f"{n:,}"
EOF
echo -e "${GREEN}User created utils.py:${NC}"
cat utils.py
echo ""

# Run second Claude prompt - have agent modify the user's file
echo -e "${BLUE}=== Step 7: Run second Claude prompt (agent updates user's file) ===${NC}"
echo "Having Claude update the user-created utils.py..."
claude --model haiku -p "Add a function called format_percentage(value) to utils.py that formats a decimal as a percentage string (e.g., 0.5 -> '50%'). Put it after the existing function." --allowedTools Edit Read

# Show what changed
echo -e "${GREEN}utils.py after second prompt:${NC}"
cat utils.py
echo ""

# Show git status after second prompt
echo -e "${BLUE}Git status after second prompt:${NC}"
git status --short
echo ""

# User makes another edit to main.py (editing agent-touched file)
echo -e "${BLUE}=== Step 8: User edits main.py (agent-touched file) ===${NC}"
cat >> main.py << 'EOF'

# User added this comment
USER_VERSION = "1.0.0"
EOF
echo -e "${GREEN}main.py after user edit:${NC}"
cat main.py
echo ""

# Check rewind points
echo -e "${BLUE}=== Step 9: Check rewind points ===${NC}"
entire rewind --list || true

# Show session state (includes PromptAttributions for debugging)
echo ""
echo -e "${BLUE}Session state files:${NC}"
GIT_DIR=$(git rev-parse --git-dir)
if [[ -d "$GIT_DIR/entire-sessions" ]]; then
    for f in "$GIT_DIR/entire-sessions"/*.json; do
        if [[ -f "$f" ]]; then
            echo -e "${GREEN}$f:${NC}"
            jq . "$f" 2>/dev/null || cat "$f"
        fi
    done
else
    echo "(no session state directory)"
fi
echo ""

# Show git status before commit
echo ""
echo -e "${BLUE}Git status before commit:${NC}"
git status --short
echo ""

# Now commit and check attribution
echo -e "${BLUE}=== Step 10: Stage and commit ===${NC}"
git add -A
git commit -m "Add random number and utility functions"

# Show the commit with trailers
echo -e "${GREEN}Commit details:${NC}"
git log -1 --format=full

# Check for Entire-Checkpoint trailer
echo ""
echo -e "${BLUE}=== Step 11: Check attribution in commit ===${NC}"
CHECKPOINT_ID=$(git log -1 --format=%B | grep "Entire-Checkpoint:" | cut -d: -f2 | tr -d ' ')
if [[ -n "$CHECKPOINT_ID" ]]; then
    echo -e "${GREEN}Found Entire-Checkpoint: $CHECKPOINT_ID${NC}"

    # Extract the sharded path: first 2 chars / remaining chars
    SHARD_PREFIX="${CHECKPOINT_ID:0:2}"
    SHARD_SUFFIX="${CHECKPOINT_ID:2}"
    METADATA_PATH="${SHARD_PREFIX}/${SHARD_SUFFIX}/metadata.json"

    echo ""
    echo -e "${BLUE}=== Step 12: Inspect metadata on entire/sessions branch ===${NC}"
    echo "Looking for metadata at: $METADATA_PATH"

    # Read metadata.json from entire/sessions branch
    if git show "entire/sessions:${METADATA_PATH}" > /dev/null 2>&1; then
        echo -e "${GREEN}Found metadata.json:${NC}"
        git show "entire/sessions:${METADATA_PATH}" | jq .

        # Extract and display attribution specifically
        echo ""
        echo -e "${BLUE}=== Step 13: Attribution Analysis ===${NC}"
        ATTRIBUTION=$(git show "entire/sessions:${METADATA_PATH}" | jq -r '.initial_attribution // empty')
        if [[ -n "$ATTRIBUTION" && "$ATTRIBUTION" != "null" ]]; then
            echo -e "${GREEN}Attribution data:${NC}"
            echo "$ATTRIBUTION" | jq .

            # Extract key values
            AGENT_LINES=$(echo "$ATTRIBUTION" | jq -r '.agent_lines')
            HUMAN_ADDED=$(echo "$ATTRIBUTION" | jq -r '.human_added')
            HUMAN_MODIFIED=$(echo "$ATTRIBUTION" | jq -r '.human_modified')
            HUMAN_REMOVED=$(echo "$ATTRIBUTION" | jq -r '.human_removed')
            TOTAL=$(echo "$ATTRIBUTION" | jq -r '.total_committed')
            PERCENTAGE=$(echo "$ATTRIBUTION" | jq -r '.agent_percentage')

            echo ""
            echo -e "${GREEN}Summary:${NC}"
            echo "  Agent lines:     $AGENT_LINES"
            echo "  Human added:     $HUMAN_ADDED"
            echo "  Human modified:  $HUMAN_MODIFIED"
            echo "  Human removed:   $HUMAN_REMOVED"
            echo "  Total committed: $TOTAL"
            echo "  Agent %:         $PERCENTAGE"
        else
            echo -e "${YELLOW}No initial_attribution in metadata${NC}"
            echo ""
            echo -e "${BLUE}Checking debug logs for attribution issues:${NC}"
            if [[ -d ".entire/logs" ]]; then
                grep -i "attribution" .entire/logs/*.log 2>/dev/null | tail -30 || echo "(no attribution logs found)"
            else
                echo "(no .entire/logs directory)"
            fi
        fi

        # Also show files_touched
        echo ""
        echo -e "${BLUE}Files touched (agent-modified):${NC}"
        git show "entire/sessions:${METADATA_PATH}" | jq -r '.files_touched[]?' 2>/dev/null || echo "(none)"

        # Show prompt attributions from session state if available
        echo ""
        echo -e "${BLUE}=== Step 14: Check prompt attributions ===${NC}"
        # List all files in the checkpoint directory
        echo "Files in checkpoint directory:"
        git ls-tree -r --name-only "entire/sessions" | grep "^${SHARD_PREFIX}/${SHARD_SUFFIX}/" | head -20

    else
        echo -e "${RED}Could not find metadata at $METADATA_PATH${NC}"
        echo "Checking what's on entire/sessions branch:"
        git ls-tree -r --name-only "entire/sessions" 2>/dev/null | head -20 || echo "(branch may not exist)"
    fi
else
    echo -e "${YELLOW}No Entire-Checkpoint trailer found (user may have removed it)${NC}"
fi

# Show rewind points summary
echo ""
echo -e "${BLUE}=== Step 15: Rewind points summary ===${NC}"
entire rewind --list | jq -r '.[] | "  \(.id[0:8])... - \(.message[0:60])"' 2>/dev/null || echo "  (no rewind points)"

# Final summary
echo ""
echo -e "${GREEN}=== Test Complete ===${NC}"
echo "Test repo location: $TEST_DIR"
echo ""
echo "What was tested:"
echo "  1. Agent added get_random_number() to main.py"
echo "  2. User created utils.py (non-agent file)"
echo "  3. Agent modified utils.py (now agent-touched)"
echo "  4. User edited main.py (agent-touched file)"
echo "  5. Commit with attribution tracking"
echo "  6. Metadata inspection on entire/sessions branch"
echo ""
echo "Expected attribution behavior:"
echo "  - main.py: agent added lines, user added 2 lines after"
echo "  - utils.py: user created (6 lines), agent added format_percentage()"
echo "  - Agent % should reflect agent lines vs total new lines"
echo ""
if [[ "$KEEP_REPO" == "true" ]]; then
    echo -e "${YELLOW}Repo kept for inspection. To clean up: rm -rf $TEST_DIR${NC}"
    echo ""
    echo "Useful inspection commands:"
    echo "  cd $TEST_DIR"
    echo "  git log entire/sessions --oneline"
    echo "  git show entire/sessions:<checkpoint-path>/metadata.json | jq ."
fi
