#!/usr/bin/env bash
# Installs the Entire CLI into a remote/Cloud Agent environment.
#
# Idempotent: no-op when `entire` is already on PATH. Safe to append to a
# Cursor `.cursor/environment.json` `install` command, a dashboard-managed
# Cloud Agent install script, or any other remote bootstrap.
set -euo pipefail

if command -v entire >/dev/null 2>&1; then
  exit 0
fi

curl -fsSL https://entire.io/install.sh | bash

BIN="${HOME}/.local/bin/entire"
if [[ ! -x "${BIN}" ]]; then
  echo "entire CLI install did not produce ${BIN}" >&2
  exit 1
fi

export PATH="${HOME}/.local/bin:${PATH}"

persist_path() {
  local rc="$1"
  if [[ -w "${rc}" || ! -e "${rc}" ]]; then
    if ! grep -qF '.local/bin' "${rc}" 2>/dev/null; then
      printf '\nexport PATH="$HOME/.local/bin:$PATH"\n' >>"${rc}"
    fi
  fi
}

persist_path "${HOME}/.bashrc"
persist_path "${HOME}/.profile"

# Hook subprocesses often inherit a minimal PATH; /usr/local/bin is on it.
if command -v sudo >/dev/null 2>&1; then
  sudo -n ln -sf "${BIN}" /usr/local/bin/entire 2>/dev/null || true
fi

if ! command -v entire >/dev/null 2>&1; then
  echo "entire is installed at ${BIN} but not on PATH" >&2
  exit 1
fi
