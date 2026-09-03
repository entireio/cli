#!/usr/bin/env bash
#
# Download E2E test artifacts from GitHub Actions.
#
# Usage: scripts/download-e2e-artifacts.sh [RUN_ID | RUN_URL | "latest"]
#   RUN_ID:  numeric GitHub Actions run ID
#   RUN_URL: full URL like https://github.com/entireio/cli/actions/runs/12345
#   "latest": most recent failed E2E run on main
#
# Outputs the absolute path of the download directory as the last line of stdout.
# All diagnostic messages go to stderr.

set -euo pipefail

log() { echo "$@" >&2; }
die() { log "ERROR: $1"; exit 1; }

# --- Validate prerequisites ---

command -v gh >/dev/null 2>&1 || die "'gh' CLI is not installed. Install from https://cli.github.com/"
gh auth status >/dev/null 2>&1 || die "'gh' is not authenticated. Run 'gh auth login' first."

# --- Parse input ---

input="${1:-}"
[ -z "$input" ] && die "Usage: $0 [RUN_ID | RUN_URL | \"latest\"]"

run_id=""

case "$input" in
  latest)
    log "Finding most recent failed E2E run on main..."
    run_id=$(gh run list -w e2e.yml --status=failure -L1 --json databaseId -q '.[0].databaseId' 2>/dev/null)
    [ -z "$run_id" ] && die "No failed E2E runs found."
    log "Found run: $run_id"
    ;;
  http*)
    # Extract run ID from URL: https://github.com/<owner>/<repo>/actions/runs/<id>
    run_id=$(echo "$input" | grep -oE '/runs/[0-9]+' | grep -oE '[0-9]+')
    [ -z "$run_id" ] && die "Could not extract run ID from URL: $input"
    log "Extracted run ID: $run_id"
    ;;
  *[!0-9]*)
    die "Invalid input: '$input'. Provide a numeric run ID, a GitHub Actions URL, or 'latest'."
    ;;
  *)
    run_id="$input"
    ;;
esac

# --- Fetch run metadata ---

log "Fetching run metadata..."
run_url=$(gh run view "$run_id" --json url -q '.url' 2>/dev/null) || die "Run $run_id not found."
commit=$(gh run view "$run_id" --json headSha -q '.headSha' 2>/dev/null) || commit="unknown"

log "Run URL: $run_url"
log "Commit:  $commit"

# --- Download artifacts ---

dest="e2e/artifacts/ci-${run_id}"

# If artifacts were already downloaded, skip re-downloading
if [ -d "$dest" ] && [ "$(ls -A "$dest" 2>/dev/null)" ]; then
  log "Artifacts already downloaded at $dest/, skipping download."
else
  mkdir -p "$dest"
  log "Downloading artifacts to $dest/ ..."
  gh run download "$run_id" --dir "$dest" 2>&1 >&2 || die "Failed to download artifacts. They may have expired (retention: 7 days)."
fi

# --- Restructure: flatten e2e-artifacts-<agent>/ wrapper dirs ---

cd "$dest"
for wrapper in e2e-artifacts-*/; do
  [ -d "$wrapper" ] || continue
  agent="${wrapper#e2e-artifacts-}"
  agent="${agent%/}"
  # Move contents up: e2e-artifacts-claude-code/* -> claude-code/
  if [ -d "$agent" ]; then
    # Agent dir already exists (shouldn't happen, but be safe)
    cp -r "$wrapper"/* "$agent"/ 2>/dev/null || true
  else
    mv "$wrapper" "$agent"
  fi
done
cd - >/dev/null

# --- Write run metadata ---

agents_found=$(cd "$dest" && find . -maxdepth 1 -mindepth 1 -type d -exec basename {} \; 2>/dev/null | sort | tr '\n' ',' | sed 's/,$//')

cat > "$dest/.run-info.json" <<EOF
{
  "run_id": "$run_id",
  "run_url": "$run_url",
  "commit": "$commit",
  "downloaded_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "agents": "${agents_found//\"/\\\"}"
}
EOF

log ""
log "Downloaded artifacts for: $agents_found"
log "Run info: $dest/.run-info.json"
log ""

# Last line of stdout: absolute path for callers to capture
abs_dest="$(cd "$dest" && pwd)"
echo "$abs_dest"
