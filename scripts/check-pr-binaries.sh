#!/usr/bin/env bash
set -euo pipefail

log() {
  echo "$@"
}

fail() {
  echo "::error::$1" >&2
  exit 1
}

base_sha="${1:-${GITHUB_BASE_SHA:-}}"
head_ref="${2:-HEAD}"
max_binary_size_bytes="${MAX_BINARY_SIZE_BYTES:-1048576}"

if [[ -z "${base_sha}" ]]; then
  fail "Missing base SHA. Pass it as the first argument or set GITHUB_BASE_SHA."
fi

if ! [[ "${max_binary_size_bytes}" =~ ^[0-9]+$ ]]; then
  fail "MAX_BINARY_SIZE_BYTES must be an integer, got: ${max_binary_size_bytes}"
fi

if ! git rev-parse --verify "${base_sha}^{commit}" >/dev/null 2>&1; then
  fail "Base commit not found locally: ${base_sha}"
fi

if ! git rev-parse --verify "${head_ref}^{commit}" >/dev/null 2>&1; then
  fail "Head commit not found locally: ${head_ref}"
fi

range="${base_sha}...${head_ref}"

log "Checking binary files in ${range}"
log "Maximum allowed binary size: ${max_binary_size_bytes} bytes"

violations=()

while IFS= read -r commit; do
  [[ -z "${commit}" ]] && continue

  while IFS=$'\t' read -r added deleted path; do
    [[ -z "${path}" ]] && continue

    # In git numstat output, binary diffs use "-" for added/deleted counts.
    if [[ "${added}" != "-" || "${deleted}" != "-" ]]; then
      continue
    fi

    blob_size="$(git cat-file -s "${commit}:${path}")"
    if (( blob_size <= max_binary_size_bytes )); then
      continue
    fi

    violations+=("${path}:${blob_size}")
  done < <(git diff-tree -m --root --no-commit-id --diff-filter=AM -r --numstat "${commit}")
done < <(git rev-list --reverse "${base_sha}..${head_ref}")

if (( ${#violations[@]} == 0 )); then
  log "No oversized binary files found."
  exit 0
fi

log "Oversized binary files detected:"
for violation in "${violations[@]}"; do
  path="${violation%%:*}"
  size="${violation##*:}"
  echo "  - ${path} (${size} bytes)"
done

fail "Pull request includes binary files larger than ${max_binary_size_bytes} bytes."
