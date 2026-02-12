#!/bin/bash
# Test script for Kilo CLI fix

cd ~/Dev/cli

echo "=== Step 1: Check current changes ==="
git diff --stat

echo ""
echo "=== Step 2: Build the project ==="
mise run build 2>&1 | tail -20

echo ""
echo "=== Step 3: Test the fix ==="
echo "Creating test settings with PreCompact hook..."
mkdir -p .claude
cat > .claude/settings.json << 'EOF'
{
  "hooks": {
    "PreCompact": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "echo 'compacting session'"
          }
        ]
      }
    ],
    "SessionStart": []
  }
}
EOF

echo "Running: ./bin/entire enable"
./bin/entire enable

echo ""
echo "=== Step 4: Verify PreCompact is preserved ==="
if grep -q "PreCompact" .claude/settings.json; then
    echo "✅ SUCCESS: PreCompact hook is preserved!"
    cat .claude/settings.json | grep -A 10 "PreCompact"
else
    echo "❌ FAIL: PreCompact hook was deleted"
fi

echo ""
echo "=== Step 5: Run tests ==="
mise run test 2>&1 | tail -30

echo ""
echo "=== Step 6: Create branch for PR ==="
git checkout -b fix-308-preserve-hooks
git add .
git commit -m "fix: preserve unknown Claude Code hook types in entire enable

The InstallHooks and UninstallHooks functions were silently dropping
any Claude Code hook types not defined in Entire's ClaudeHooks struct.
This caused hooks like PreCompact, Notification, SubagentStart, etc.
to be deleted when running 'entire enable'.

Fix by using rawClaudeHooks (map[string]json.RawMessage) to preserve
all hook types, similar to how rawSettings preserves unknown top-level
fields. Only modify the 6 known hook types while keeping all others.

Fixes #308"

echo ""
echo "=== Ready to push! ==="
echo "Run: git push origin fix-308-preserve-hooks"
