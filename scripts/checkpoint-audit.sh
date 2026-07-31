#!/usr/bin/env bash
set -euo pipefail

# Audit checkpoint availability on the checkpoint remote.
#
# Walks every commit created within a time window across all branches, extracts
# each `Entire-Checkpoint:` trailer, and checks whether the matching checkpoint
# ref exists on the checkpoint remote (github.com/<CHECKPOINT_REPO>). Any commit
# whose checkpoint ref is missing from the remote is reported with context and
# makes the script exit non-zero.
#
# This catches the failure mode where a commit reaches origin but its git-refs
# checkpoint ref (refs/entire/checkpoints/<shard>/<id>) was never pushed to the
# checkpoint remote — the checkpoint is then unrecoverable on any other machine
# (e.g. `entire trail resume` / `entire explain` fail with "checkpoint not found").
#
# Trailer parsing and ref-set membership use git's own primitives rather than
# hand-rolled regex/shard math: `git show --format='%(trailers:...)'` parses
# trailers (handling squash-merge commits with multiple checkpoint trailers), and
# the checkpoint ID is simply the ref leaf, so membership is a plain string-set
# lookup that works for both legacy-hex and ULID IDs.
#
# Env (all optional except the token for a private remote):
#   ENTIRE_CHECKPOINT_TOKEN  GitHub token with read access to the checkpoint repo.
#                            Sent as an RFC 7617 basic auth header (matching the
#                            CLI), so it never appears in a remote URL.
#   CHECKPOINT_REPO          owner/repo of the checkpoint remote
#                            (default: entireio/cli-checkpoints).
#   AUDIT_WINDOW             git `--since` window (default: "24 hours ago").
#   AUDIT_REPORT_FILE        markdown report sink (default: checkpoint-audit-report.md).
#   AUDIT_JSON_FILE          machine-readable report sink (default: checkpoint-audit-report.json).
#   GITHUB_STEP_SUMMARY      if set, the markdown report is appended to it.
#
# Exit codes: 0 = all present, 1 = one or more checkpoints missing, 2 = setup or
# remote error (a broken remote must never read as "0 missing").

CHECKPOINT_REPO="${CHECKPOINT_REPO:-entireio/cli-checkpoints}"
AUDIT_WINDOW="${AUDIT_WINDOW:-24 hours ago}"
AUDIT_REPORT_FILE="${AUDIT_REPORT_FILE:-checkpoint-audit-report.md}"
AUDIT_JSON_FILE="${AUDIT_JSON_FILE:-checkpoint-audit-report.json}"
TOKEN="${ENTIRE_CHECKPOINT_TOKEN:-}"

CHECKPOINT_URL="https://github.com/${CHECKPOINT_REPO}.git"

REMOTE_IDS_FILE=$(mktemp)
ROWS_FILE=$(mktemp)
trap 'rm -f "$REMOTE_IDS_FILE" "$ROWS_FILE"' EXIT

# json_str emits a JSON string literal, escaping backslashes and double quotes.
# Author names and subjects are single-line, so control-char escaping is not needed.
json_str() {
  local s=${1//\\/\\\\}
  s=${s//\"/\\\"}
  printf '"%s"' "$s"
}

# 1. Enumerate the checkpoint refs present on the remote (names only, no object
#    transfer). The ID is the ref leaf: refs/entire/checkpoints/<shard>/<ID>.
echo "Enumerating checkpoint refs on ${CHECKPOINT_REPO} ..." >&2
if [ -n "$TOKEN" ]; then
  auth_b64=$(printf 'x-access-token:%s' "$TOKEN" | base64 | tr -d '\n')
  remote_refs=$(git -c "http.extraheader=AUTHORIZATION: basic ${auth_b64}" \
    ls-remote "$CHECKPOINT_URL" 'refs/entire/checkpoints/*') || {
    echo "::error::failed to ls-remote ${CHECKPOINT_REPO} (check ENTIRE_CHECKPOINT_TOKEN and repo access)" >&2
    exit 2
  }
else
  remote_refs=$(git ls-remote "$CHECKPOINT_URL" 'refs/entire/checkpoints/*') || {
    echo "::error::failed to ls-remote ${CHECKPOINT_REPO} (no ENTIRE_CHECKPOINT_TOKEN set; is the repo private?)" >&2
    exit 2
  }
fi

printf '%s\n' "$remote_refs" \
  | awk '$2 ~ /^refs\/entire\/checkpoints\// { id = $2; sub(/.*\//, "", id); print id }' \
  | sort -u > "$REMOTE_IDS_FILE"
remote_count=$(grep -c . "$REMOTE_IDS_FILE" || true)

# 2. Candidate commits: every branch commit in the window (local heads + remotes),
#    de-duplicated while preserving order.
commits=$(git log --branches --remotes --since="$AUDIT_WINDOW" --format='%H' | awk '!seen[$0]++')

# 3. For each commit, diff its checkpoint trailers against the remote set.
commit_count=0
cp_count=0
missing_count=0
while IFS= read -r sha; do
  [ -z "$sha" ] && continue
  commit_count=$((commit_count + 1))
  cps=$(git show -s --format='%(trailers:key=Entire-Checkpoint,valueonly=true)' "$sha")
  while IFS= read -r cp; do
    cp="${cp//[[:space:]]/}"
    [ -z "$cp" ] && continue
    cp_count=$((cp_count + 1))
    if grep -Fxq "$cp" "$REMOTE_IDS_FILE"; then
      continue
    fi
    missing_count=$((missing_count + 1))
    meta=$(git show -s --format='%h%x1f%an%x1f%aI%x1f%s' "$sha")
    short=${meta%%$'\x1f'*}; meta=${meta#*$'\x1f'}
    author=${meta%%$'\x1f'*}; meta=${meta#*$'\x1f'}
    cdate=${meta%%$'\x1f'*}; subject=${meta#*$'\x1f'}
    branches=$(git branch -a --contains "$sha" --format='%(refname:short)' 2>/dev/null \
      | sed -e 's#^remotes/##' -e 's#^origin/##' \
      | grep -v '^entire/' \
      | awk 'NF && !s[$0]++' \
      | paste -sd', ' -)
    printf '%s\x1e%s\x1e%s\x1e%s\x1e%s\x1e%s\n' \
      "$cp" "$short" "$author" "${branches:-?}" "$cdate" "$subject" >> "$ROWS_FILE"
  done <<EOF
$cps
EOF
done <<EOF
$commits
EOF

# 4. Render the markdown report (stdout + file + optional step summary).
{
  echo "# Checkpoint availability audit"
  echo
  echo "- Window: commits since \`${AUDIT_WINDOW}\`"
  echo "- Checkpoint remote: \`${CHECKPOINT_REPO}\` (${remote_count} refs present)"
  echo "- Scanned: ${commit_count} commit(s), ${cp_count} checkpoint trailer(s)"
  echo "- **Missing from remote: ${missing_count}**"
  echo
  if [ "$missing_count" -gt 0 ]; then
    echo "| Checkpoint | Commit | Author | Branch(es) | Date | Subject |"
    echo "|---|---|---|---|---|---|"
    while IFS=$'\x1e' read -r cp short author branches cdate subject; do
      subject=${subject//|/\\|}
      echo "| \`${cp}\` | \`${short}\` | ${author} | ${branches} | ${cdate} | ${subject} |"
    done < "$ROWS_FILE"
  else
    echo "All checkpoints referenced in the window are present on the remote. :white_check_mark:"
  fi
} | tee "$AUDIT_REPORT_FILE"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  cat "$AUDIT_REPORT_FILE" >> "$GITHUB_STEP_SUMMARY"
fi

# 5. Machine-readable report for the artifact.
{
  echo "["
  first=1
  while IFS=$'\x1e' read -r cp short author branches cdate subject; do
    if [ "$first" -eq 1 ]; then first=0; else echo ","; fi
    printf '  {"checkpoint":%s,"commit":%s,"author":%s,"branches":%s,"date":%s,"subject":%s}' \
      "$(json_str "$cp")" "$(json_str "$short")" "$(json_str "$author")" \
      "$(json_str "$branches")" "$(json_str "$cdate")" "$(json_str "$subject")"
  done < "$ROWS_FILE"
  echo
  echo "]"
} > "$AUDIT_JSON_FILE"

if [ "$missing_count" -gt 0 ]; then
  echo "::error::${missing_count} checkpoint(s) missing from ${CHECKPOINT_REPO}" >&2
  exit 1
fi
echo "All ${cp_count} checkpoint(s) present on ${CHECKPOINT_REPO}." >&2
exit 0
