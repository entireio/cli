#!/bin/bash
# Remote environment setup for Claude Code web sessions
# This script installs the Entire CLI, configures DNS, and installs mise in
# cloud containers

set -e

# Only run in remote (web/cloud) environments
if [ "$CLAUDE_CODE_REMOTE" != "true" ]; then
  exit 0
fi

echo "Setting up remote environment..."

# 1. Install the latest released Entire CLI so the committed agent hooks track
# this session. Fresh containers have no binary, and without one every hook
# exits silently. Once the binary exists, the first lifecycle hook installs the
# git hooks through EnsureSetup, so `entire enable` is not needed here. Runs
# first and never fails the script, so a later setup step cannot skip it.
install_entire() {
  local arch tmp base
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) echo "Skipping Entire CLI install: unsupported architecture $(uname -m)"; return 1 ;;
  esac

  tmp=$(mktemp -d)
  base="https://github.com/entireio/cli/releases/latest/download"
  (
    cd "$tmp" &&
      curl -fsSLO "$base/entire_linux_${arch}.tar.gz" &&
      curl -fsSLO "$base/checksums.txt" &&
      grep " entire_linux_${arch}.tar.gz\$" checksums.txt | sha256sum -c --status - &&
      tar xzf "entire_linux_${arch}.tar.gz" entire git-remote-entire &&
      mkdir -p "$HOME/.local/bin" &&
      install -m 755 entire git-remote-entire "$HOME/.local/bin/"
  )
  local status=$?
  rm -rf "$tmp"
  return $status
}

if ! command -v entire &> /dev/null; then
  echo "Installing Entire CLI..."
  if install_entire; then
    export PATH="$HOME/.local/bin:$PATH"
    if [ -n "$CLAUDE_ENV_FILE" ]; then
      printf 'export PATH=%q:"$PATH"\n' "$HOME/.local/bin" >> "$CLAUDE_ENV_FILE"
    fi
    entire version | head -n 1
  else
    echo "Warning: Entire CLI install failed; this session will not be tracked"
  fi
fi

# 2. Configure DNS (required in web containers)
echo "Configuring DNS..."
echo "nameserver 8.8.8.8" | sudo tee /etc/resolv.conf > /dev/null

# 3. Install mise if not already installed
if ! command -v mise &> /dev/null; then
  echo "Installing mise..."
  curl -fsSL https://mise.run | sh

  # Add mise to PATH for this script
  export PATH="$HOME/.local/bin:$PATH"
fi

# 4. Trust mise config and install tools
echo "Installing project tools..."
cd "$CLAUDE_PROJECT_DIR"
mise trust
mise install

# 5. Persist mise activation and CLAUDE_PROJECT_DIR for subsequent commands
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
