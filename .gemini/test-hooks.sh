#!/bin/bash
# Test script to verify Gemini CLI hooks work correctly
# Run from the cli directory: .gemini/test-hooks.sh

set -e

cd "$(dirname "$0")/.."

echo "=== Testing Gemini CLI Hook Handlers ==="
echo ""

# Create a temp directory for test transcript
TEMP_DIR=$(mktemp -d)
TRANSCRIPT_FILE="$TEMP_DIR/transcript.json"
echo '{"messages": [{"role": "user", "content": "test prompt"}]}' > "$TRANSCRIPT_FILE"

# Test 1: Session Start Hook
echo "1. Testing session-start hook..."
SESSION_START_INPUT=$(cat <<EOF
{
  "session_id": "test-session-$(date +%s)",
  "transcript_path": "$TRANSCRIPT_FILE",
  "cwd": "$(pwd)",
  "hook_event_name": "session_start",
  "timestamp": "$(date -Iseconds)",
  "source": "startup"
}
EOF
)

echo "$SESSION_START_INPUT" | go run ./cmd/entire/main.go hooks gemini session-start && echo "   ✓ session-start passed" || echo "   ✗ session-start failed"

# Test 2: Before Tool Hook
echo ""
echo "2. Testing before-tool hook..."
BEFORE_TOOL_INPUT=$(cat <<EOF
{
  "session_id": "test-session-$(date +%s)",
  "transcript_path": "$TRANSCRIPT_FILE",
  "cwd": "$(pwd)",
  "hook_event_name": "before_tool",
  "timestamp": "$(date -Iseconds)",
  "tool_name": "write_file",
  "tool_input": {"file_path": "test.txt", "content": "hello"}
}
EOF
)

echo "$BEFORE_TOOL_INPUT" | go run ./cmd/entire/main.go hooks gemini before-tool && echo "   ✓ before-tool passed" || echo "   ✗ before-tool failed"

# Test 3: After Tool Hook
echo ""
echo "3. Testing after-tool hook..."
AFTER_TOOL_INPUT=$(cat <<EOF
{
  "session_id": "test-session-$(date +%s)",
  "transcript_path": "$TRANSCRIPT_FILE",
  "cwd": "$(pwd)",
  "hook_event_name": "after_tool",
  "timestamp": "$(date -Iseconds)",
  "tool_name": "write_file",
  "tool_input": {"file_path": "test.txt", "content": "hello"},
  "tool_response": {"success": true}
}
EOF
)

echo "$AFTER_TOOL_INPUT" | go run ./cmd/entire/main.go hooks gemini after-tool && echo "   ✓ after-tool passed" || echo "   ✗ after-tool failed"

# Test 4: Session End Hook
echo ""
echo "4. Testing session-end hook..."
SESSION_END_INPUT=$(cat <<EOF
{
  "session_id": "test-session-$(date +%s)",
  "transcript_path": "$TRANSCRIPT_FILE",
  "cwd": "$(pwd)",
  "hook_event_name": "session_end",
  "timestamp": "$(date -Iseconds)",
  "reason": "exit"
}
EOF
)

echo "$SESSION_END_INPUT" | go run ./cmd/entire/main.go hooks gemini session-end && echo "   ✓ session-end passed" || echo "   ✗ session-end failed"

# Cleanup
rm -rf "$TEMP_DIR"

echo ""
echo "=== Hook Tests Complete ==="
