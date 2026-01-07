#!/bin/bash
# Test harness for Entire CLI strategy testing
# Usage: test-harness.sh <step> [args...]
#
# This script allows step-by-step execution of the test flow.
# Grant permission once, then execute individual steps.

set -e # Exit on error

REPO_DIR="${REPO_DIR:-/tmp/entire-test-repo}"
BIN_PATH="${BIN_PATH:-/tmp/entire-bin}"
STRATEGY="${STRATEGY:-manual-commit}"
# Use proper UUID session ID format (matches Claude Code format)
SESSION_ID="${SESSION_ID:-$(uuidgen | tr '[:upper:]' '[:lower:]')}"
TRANSCRIPT_DIR="$REPO_DIR/.claude-test"

# Export for reuse across steps
export SESSION_ID

step=$1
shift # Remove step name from args

case "$step" in
setup-repo)
  echo "==> Setting up test repository..."
  rm -rf "$REPO_DIR"
  mkdir -p "$REPO_DIR"
  cd "$REPO_DIR"

  git init
  git config user.email "test@test.com"
  git config user.name "Test User"
  echo "# Test Repo" >README.md
  git add README.md
  git commit -m "Initial commit"

  echo "Repository created: $REPO_DIR"
  ;;

configure-strategy)
  echo "==> Configuring strategy: $STRATEGY"
  cd "$REPO_DIR"

  mkdir -p .entire
  printf ".entire/\n.claude-test/\n" >.gitignore
  git add .gitignore
  "$BIN_PATH" enable --agent claude-code --strategy "$STRATEGY"
  git add .entire
  git add .claude
  git commit -m "Configure Entire with $STRATEGY strategy"

  git checkout -b feature/test-session

  echo "Strategy configured: $STRATEGY"
  ;;

start-session)
  echo "==> Starting session: $SESSION_ID"
  cd "$REPO_DIR"
  mkdir -p "$TRANSCRIPT_DIR"

  echo "{\"session_id\": \"$SESSION_ID\"}" |
    ENTIRE_TEST_CLAUDE_PROJECT_DIR="$TRANSCRIPT_DIR" \
      "$BIN_PATH" hooks claude-code user-prompt-submit

  echo "Session started: $SESSION_ID"
  echo "Transcript dir: $TRANSCRIPT_DIR"
  ;;

create-files)
  echo "==> Creating test files..."
  cd "$REPO_DIR"

  echo "console.log('hello world');" >app.js
  echo "function greet() { return 'hi'; }" >>app.js

  echo "Created: app.js"
  ;;

create-transcript)
  echo "==> Creating transcript..."
  cd "$REPO_DIR"

  # Use proper Claude Code format with timestamps and UUIDs
  TIMESTAMP=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
  cat >"$TRANSCRIPT_DIR/transcript.jsonl" <<EOF
{"type":"user","uuid":"$(uuidgen | tr '[:upper:]' '[:lower:]')","sessionId":"$SESSION_ID","timestamp":"$TIMESTAMP","message":{"role":"user","content":"Add a hello world function"}}
{"type":"assistant","uuid":"$(uuidgen | tr '[:upper:]' '[:lower:]')","sessionId":"$SESSION_ID","timestamp":"$TIMESTAMP","message":{"role":"assistant","content":[{"type":"text","text":"I'll add a hello world function to app.js"}]}}
EOF

  echo "Created: $TRANSCRIPT_DIR/transcript.jsonl"
  ;;

stop-session)
  echo "==> Stopping session (creating checkpoint)..."
  cd "$REPO_DIR"

  echo "{\"session_id\": \"$SESSION_ID\", \"transcript_path\": \"$TRANSCRIPT_DIR/transcript.jsonl\"}" |
    ENTIRE_TEST_CLAUDE_PROJECT_DIR="$TRANSCRIPT_DIR" \
      "$BIN_PATH" hooks claude-code stop

  echo "Session stopped, checkpoint created"
  ;;

verify-commit)
  echo "==> Verifying active branch commit..."
  cd "$REPO_DIR"

  git log -1 --format="%B"
  ;;

verify-session-state)
  echo "==> Verifying session state..."
  cd "$REPO_DIR"

  if ls .git/entire-sessions/*.json 1>/dev/null 2>&1; then
    echo "✓ Session state files exist:"
    ls -la .git/entire-sessions/
  else
    echo "✗ No session state files found"
    exit 1
  fi
  ;;

verify-shadow-branch)
  echo "==> Verifying shadow branch..."
  cd "$REPO_DIR"

  if git branch -a | grep -E "entire/[0-9a-f]"; then
    echo "✓ Shadow branch exists"
  else
    echo "Note: No shadow branch (expected for auto-commit strategy)"
  fi
  ;;

verify-metadata-branch)
  echo "==> Verifying metadata branch..."
  cd "$REPO_DIR"

  if git branch -a | grep "entire/sessions"; then
    echo "✓ Metadata branch exists"
    git show entire/sessions --stat | head -20
  else
    echo "✗ Metadata branch not found"
    exit 1
  fi
  ;;

list-rewind-points)
  echo "==> Listing rewind points..."
  cd "$REPO_DIR"

  "$BIN_PATH" rewind --list
  ;;

create-changes)
  echo "==> Creating additional changes for rewind test..."
  cd "$REPO_DIR"

  echo "// More changes" >>app.js
  echo "extra content" >extra.js

  echo "Modified: app.js"
  echo "Created: extra.js"
  ;;

rewind)
  checkpoint_id="${1:-}"
  if [ -z "$checkpoint_id" ]; then
    echo "Usage: $0 rewind <checkpoint-id>"
    exit 1
  fi

  echo "==> Rewinding to checkpoint: $checkpoint_id"
  cd "$REPO_DIR"

  git stash
  "$BIN_PATH" rewind --to "$checkpoint_id"

  echo "Rewound to: $checkpoint_id"
  ;;

verify-rewind)
  echo "==> Verifying rewind..."
  cd "$REPO_DIR"

  echo "Contents of app.js:"
  cat app.js

  if [ -f extra.js ]; then
    echo "✗ FAIL: extra.js should not exist after rewind"
    exit 1
  else
    echo "✓ PASS: extra.js correctly removed"
  fi
  ;;

cleanup)
  echo "==> Cleaning up..."
  rm -rf "$REPO_DIR" "$BIN_PATH"
  echo "Cleaned up test environment"
  ;;

info)
  echo "==> Test Environment Info"
  echo "Repository: $REPO_DIR"
  echo "Binary: $BIN_PATH"
  echo "Strategy: $STRATEGY"
  echo "Session ID: $SESSION_ID"
  echo "Transcript: $TRANSCRIPT_DIR"
  ;;

*)
  echo "Unknown step: $step"
  echo ""
  echo "Available steps:"
  echo "  setup-repo             - Create test repository"
  echo "  configure-strategy     - Configure Entire with strategy"
  echo "  start-session          - Start Claude session"
  echo "  create-files           - Create test files"
  echo "  create-transcript      - Create session transcript"
  echo "  stop-session           - Stop session (create checkpoint)"
  echo "  verify-commit          - Verify active branch commit"
  echo "  verify-session-state   - Verify session state files"
  echo "  verify-shadow-branch   - Verify shadow branch exists"
  echo "  verify-metadata-branch - Verify metadata branch exists"
  echo "  list-rewind-points     - List available rewind points"
  echo "  create-changes         - Create changes for rewind test"
  echo "  rewind <id>            - Rewind to checkpoint ID"
  echo "  verify-rewind          - Verify rewind worked"
  echo "  cleanup                - Clean up test environment"
  echo "  info                   - Show environment info"
  echo ""
  echo "Prerequisites:"
  echo "  Build the CLI first: go build -o /tmp/entire-bin ./cmd/entire"
  echo ""
  echo "Environment variables:"
  echo "  REPO_DIR=/tmp/entire-test-repo"
  echo "  BIN_PATH=/tmp/entire-bin (must exist before running)"
  echo "  STRATEGY=manual-commit"
  echo "  SESSION_ID=uuid (auto-generated)"
  exit 1
  ;;
esac
