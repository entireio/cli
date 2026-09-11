#!/usr/bin/env bash
# Cloud Agent install script for the Entire CLI.
#
# Idempotent: safe to run repeatedly and against a cached/partially-prepared
# snapshot. It prepares the mise-pinned toolchain, downloads Go modules, and
# builds the CLI. It does NOT start any long-running process.
set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

log() { printf '\n\033[1m[install]\033[0m %s\n' "$*"; }

# 1. Ensure git supports the reftable ref-format. The test suite creates a
#    reftable repository with `git init --ref-format=reftable`, and Ubuntu 24.04
#    ships git 2.43 (ref-format was stabilised in 2.45). Probe the actual
#    feature rather than parsing the version string (packaged/backported builds
#    have unreliable version strings), and upgrade from the git-core PPA when it
#    is missing (or when git itself is absent).
git_supports_reftable() {
  command -v git >/dev/null 2>&1 || return 1
  local probe ok=1
  probe="$(mktemp -d)"
  git init -q --ref-format=reftable "${probe}" >/dev/null 2>&1 && ok=0
  rm -rf "${probe}"
  return "${ok}"
}

if ! git_supports_reftable; then
  log "Installing/upgrading git for reftable support (have $(git --version 2>/dev/null | awk '{print $3}' || echo none))"
  sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends software-properties-common
  sudo add-apt-repository -y ppa:git-core/ppa
  sudo apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y git
fi
log "git $(git --version | awk '{print $3}') (reftable supported)"

# 2. Ensure mise (task runner + toolchain manager) is installed and on PATH.
if ! command -v mise >/dev/null 2>&1; then
  if [ ! -x "${HOME}/.local/bin/mise" ]; then
    log "Installing mise"
    curl -fsSL https://mise.run | sh
  fi
  export PATH="${HOME}/.local/bin:${PATH}"
fi
log "mise $(mise --version)"

# 3. Make mise + its shims available in interactive agent terminals.
BASHRC="${HOME}/.bashrc"
if [ -w "${BASHRC}" ] || [ ! -e "${BASHRC}" ]; then
  if ! grep -q 'mise activate bash' "${BASHRC}" 2>/dev/null; then
    cat >> "${BASHRC}" <<'MISE_ACTIVATE'
export PATH="$HOME/.local/bin:$PATH"
command -v mise >/dev/null 2>&1 && eval "$(mise activate bash)"
MISE_ACTIVATE
  fi
fi

# 4. Install the pinned toolchain (Go, golangci-lint, gotestsum, shellcheck,
#    tmux, and the roger-roger E2E helper binaries) declared in mise.toml.
mise trust --yes
mise install
eval "$(mise activate bash --shims)"

# 5. Download Go modules and build the CLI to prove the toolchain works.
log "Downloading Go modules"
go mod download

log "Building the entire CLI"
go build -o entire ./cmd/entire/
./entire version

# 6. Put `entire` on PATH. This repo dogfoods Entire: it commits agent hook
#    configs (.cursor/hooks.json, .claude/settings.json, ...) that invoke the
#    bare `entire` command and no-op when it is not found. Making the built
#    binary discoverable is all the environment needs — Entire installs its own
#    git hooks on the first turn-start hook (strategy.EnsureSetup reinstalls them
#    when absent). Prefer /usr/local/bin (on the standard PATH hooks run with);
#    fall back to ~/.local/bin when sudo is unavailable.
log "Putting entire on PATH"
if ! sudo -n ln -sf "${REPO_ROOT}/entire" /usr/local/bin/entire 2>/dev/null; then
  mkdir -p "${HOME}/.local/bin"
  ln -sf "${REPO_ROOT}/entire" "${HOME}/.local/bin/entire"
fi
log "entire on PATH: $(command -v entire || echo MISSING)"

log "Environment ready."
