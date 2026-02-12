#!/bin/bash
# Backfill script for applying Simili Bot triage to existing open issues
# Usage: ./scripts/simili-backfill.sh

set -e

REPO="entireio/cli"
CONFIG=".github/simili.yaml"

echo "🔍 Fetching open issues from $REPO..."

# Fetch all open issues
gh issue list --repo "$REPO" --state open --json number,title,body,author,labels,createdAt --limit 1000 > /tmp/issues.json

ISSUE_COUNT=$(jq '. | length' /tmp/issues.json)
echo "📊 Found $ISSUE_COUNT open issues"

if [ "$ISSUE_COUNT" -eq 0 ]; then
  echo "✅ No issues to process"
  exit 0
fi

echo "🤖 Processing issues with Simili Bot..."

# Process each issue
jq -c '.[]' /tmp/issues.json | while read -r issue; do
  NUMBER=$(echo "$issue" | jq -r '.number')
  TITLE=$(echo "$issue" | jq -r '.title')
  
  echo "  Processing #$NUMBER: $TITLE"
  
  # Create temporary issue file
  echo "$issue" > "/tmp/issue-$NUMBER.json"
  
  # Run simili process (requires simili CLI to be installed)
  if command -v simili &> /dev/null; then
    simili process --issue "/tmp/issue-$NUMBER.json" --config "$CONFIG" || echo "    ⚠️  Failed to process #$NUMBER"
  else
    echo "    ⚠️  simili CLI not found. Install with: gh extension install similigh/simili-bot"
    exit 1
  fi
  
  # Cleanup
  rm "/tmp/issue-$NUMBER.json"
  
  # Rate limit: sleep 2s between issues
  sleep 2
done

echo "✅ Backfill complete!"
