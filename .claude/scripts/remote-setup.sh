#!/bin/bash
# Remote environment setup for Claude Code web sessions
# Configures DNS, installs mise + project tools, and installs the Entire CLI
# (plus its git hooks) so checkpoints are recorded in cloud containers.

set -e

# Only run in remote (web/cloud) environments
if [ "$CLAUDE_CODE_REMOTE" != "true" ]; then
  exit 0
fi

echo "Setting up remote environment..."

# install.sh and mise.run both install into ~/.local/bin. Put it on PATH up
# front so every later step (and the PATH persisted below) can see them.
export PATH="$HOME/.local/bin:$PATH"

cd "$CLAUDE_PROJECT_DIR"

# 1. Configure DNS (required in web containers)
echo "Configuring DNS..."
echo "nameserver 8.8.8.8" | sudo tee /etc/resolv.conf > /dev/null

# 2. Install mise if not already installed
if ! command -v mise &> /dev/null; then
  echo "Installing mise..."
  curl -fsSL https://mise.run | sh
fi

# 3. Install the Entire CLI
#
# Without the binary on PATH every Entire hook in .claude/settings.json exits
# 0 as a no-op, so a cloud session silently records no checkpoints. This runs
# before `mise install` so checkpointing does not depend on the Go toolchain
# download succeeding. Set ENTIRE_INSTALL_CHANNEL=nightly for prereleases.
if command -v entire &> /dev/null; then
  echo "Entire CLI already installed: $(entire version 2>/dev/null | head -n 1)"
else
  echo "Installing Entire CLI..."
  if ! bash scripts/install.sh --channel "${ENTIRE_INSTALL_CHANNEL:-stable}"; then
    echo "Warning: Entire CLI install failed; this session will not record checkpoints." >&2
  fi
fi

# 4. Install the Entire git hooks
#
# The container clones the repo fresh, so .git/hooks holds nothing but the
# samples even though .entire/settings.json is committed with enabled: true.
# Without them a commit never condenses its checkpoint and a push never carries
# checkpoint refs. `entire enable` is idempotent on an already-configured repo:
# it installs the missing hooks and leaves the working tree untouched.
#
# Note: checkpoints are only *created* locally here. Pushing them to the
# dedicated checkpoint remote in .entire/settings.json needs that repo in the
# session's authorized repository set, or the git proxy denies the push.
if command -v entire &> /dev/null; then
  echo "Installing Entire git hooks..."
  if ! entire enable; then
    echo "Warning: 'entire enable' failed; Entire git hooks may be missing." >&2
  fi
fi

# 5. Trust mise config and install tools
echo "Installing project tools..."
mise trust
mise install

# 6. Persist mise activation and CLAUDE_PROJECT_DIR for subsequent commands
# Write exports to CLAUDE_ENV_FILE so they're available for later hooks
if [ -n "$CLAUDE_ENV_FILE" ]; then
  echo "Persisting mise environment..."

  # Export CLAUDE_PROJECT_DIR for entire hooks that need it
  # Use printf %q to safely escape the value for shell sourcing
  if [ -n "$CLAUDE_PROJECT_DIR" ]; then
    printf 'export CLAUDE_PROJECT_DIR=%q\n' "$CLAUDE_PROJECT_DIR" >> "$CLAUDE_ENV_FILE"
  fi

  # Capture exports before and after mise activation, then write only the diff.
  # PATH is part of that diff, and already carries ~/.local/bin from the export
  # at the top of this script, so later hooks resolve `entire`.
  ENV_BEFORE=$(export -p | sort)
  eval "$(mise activate bash)"
  ENV_AFTER=$(export -p | sort)
  comm -13 <(echo "$ENV_BEFORE") <(echo "$ENV_AFTER") >> "$CLAUDE_ENV_FILE"
fi

echo "Remote environment setup complete!"
