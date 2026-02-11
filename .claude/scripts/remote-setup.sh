#!/bin/bash
# Remote environment setup for Claude Code web sessions
# This script configures DNS and installs mise in cloud containers

set -e

# Only run in remote (web/cloud) environments
if [ "$CLAUDE_CODE_REMOTE" != "true" ]; then
  exit 0
fi

echo "Setting up remote environment..."

# 1. Configure DNS (required in web containers)
echo "Configuring DNS..."
echo "nameserver 8.8.8.8" | sudo tee /etc/resolv.conf > /dev/null

# 2. Install mise if not already installed
if ! command -v mise &> /dev/null; then
  echo "Installing mise..."
  curl -fsSL https://mise.run | sh

  # Add mise to PATH for this script
  export PATH="$HOME/.local/bin:$PATH"
fi

# 3. Trust mise config and install tools
echo "Installing project tools..."
cd "$CLAUDE_PROJECT_DIR"
mise trust
mise install

# 4. Persist mise activation and CLAUDE_PROJECT_DIR for subsequent commands
# Write exports to CLAUDE_ENV_FILE so they're available for later hooks
if [ -n "$CLAUDE_ENV_FILE" ]; then
  echo "Persisting mise environment..."

  # Export CLAUDE_PROJECT_DIR for entire hooks that need it
  # Use printf %q to safely escape the value for shell sourcing
  if [ -n "$CLAUDE_PROJECT_DIR" ]; then
    printf 'export CLAUDE_PROJECT_DIR=%q\n' "$CLAUDE_PROJECT_DIR" >> "$CLAUDE_ENV_FILE"
  fi

  # Capture exports before and after mise activation, then write only the diff
  ENV_BEFORE=$(export -p | sort)
  eval "$(mise activate bash)"
  ENV_AFTER=$(export -p | sort)
  comm -13 <(echo "$ENV_BEFORE") <(echo "$ENV_AFTER") >> "$CLAUDE_ENV_FILE"
fi

echo "Remote environment setup complete!"
