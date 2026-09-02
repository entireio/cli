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

  # Only ignore test-specific dirs in root .gitignore
  printf ".claude-test/\n" >.gitignore
  git add .gitignore
  # manual-commit is the only strategy; enable no longer takes --strategy.
  "$BIN_PATH" enable --agent claude-code
  git add .entire/settings.json .entire/.gitignore
  git add .claude 2>/dev/null || true
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
{"type":"tool_result","tool_use_id":"toolu_test1","content":"File written successfully","timestamp":"$TIMESTAMP"}
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
    echo "Note: No shadow branch"
  fi
  ;;

verify-metadata-branch)
  echo "==> Verifying checkpoint storage..."
  cd "$REPO_DIR"

  # `enable` defaults to the git-refs backend: condensed checkpoints live
  # under refs/entire/checkpoints/<shard>/<ULID>. The entire/checkpoints/v1
  # branch is the git-branch backend, still valid when settings select it.
  # Prefix form, no glob: the refs sit two levels deep and for-each-ref's
  # '*' does not cross '/', so 'refs/entire/checkpoints/*' matches nothing.
  refs=$(git for-each-ref --format='%(refname)' 'refs/entire/checkpoints')
  if [ -n "$refs" ]; then
    echo "✓ Checkpoint refs exist (git-refs backend)"
    echo "$refs" | head -5
    first_ref=$(echo "$refs" | head -1)
    git ls-tree -r "$first_ref" --name-only | head -10
  elif git branch -a | grep "entire/checkpoints/v1"; then
    echo "✓ Metadata branch exists (git-branch backend)"
    git show entire/checkpoints/v1 --stat | head -20
  else
    echo "✗ No checkpoint storage found (neither refs/entire/checkpoints/* nor entire/checkpoints/v1)"
    echo "  Checkpoints condense on user commits — run the commit-changes step first."
    exit 1
  fi
  ;;

list-pending-checkpoints)
  echo "==> Listing pending checkpoints..."
  cd "$REPO_DIR"

  "$BIN_PATH" checkpoint list --pending --json
  ;;

create-changes)
  echo "==> Creating post-checkpoint changes..."
  cd "$REPO_DIR"

  echo "// More changes" >>app.js
  echo "extra content" >extra.js

  echo "Modified: app.js"
  echo "Created: extra.js"
  ;;

commit-changes)
  echo "==> Committing session changes (triggers post-commit condensation)..."
  cd "$REPO_DIR"

  # The post-commit hook resolves `entire` from PATH; point it at BIN_PATH so
  # condensation runs the binary under test, not an installed release.
  mkdir -p "$REPO_DIR/.test-bin"
  ln -sf "$BIN_PATH" "$REPO_DIR/.test-bin/entire"
  git add -A
  PATH="$REPO_DIR/.test-bin:$PATH" git commit -m "Session changes"

  echo "Committed; checkpoints condense into storage on this commit"
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
  echo "  setup-repo               - Create test repository"
  echo "  configure-strategy       - Configure Entire with strategy"
  echo "  start-session            - Start Claude session"
  echo "  create-files             - Create test files"
  echo "  create-transcript        - Create session transcript"
  echo "  stop-session             - Stop session (create checkpoint)"
  echo "  verify-commit            - Verify active branch commit"
  echo "  verify-session-state     - Verify session state files"
  echo "  verify-shadow-branch     - Verify shadow branch exists"
  echo "  commit-changes           - Commit session changes (condenses checkpoints)"
  echo "  verify-metadata-branch   - Verify checkpoint storage (refs or v1 branch)"
  echo "  list-pending-checkpoints - List pending (not yet condensed) checkpoints"
  echo "  create-changes           - Create changes on top of the checkpoint"
  echo "  cleanup                  - Clean up test environment"
  echo "  info                     - Show environment info"
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
