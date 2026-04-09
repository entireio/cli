#!/usr/bin/env bash
set -euo pipefail

# Calculate the next nightly release tag based on the latest stable tag and HEAD.
# Outputs the tag name to stdout, or exits 0 silently if a nightly already exists for HEAD.
#
# Tag format: v<major>.<minor>.<patch+1>-nightly.<YYYYMMDDhhmm>.<short-commit>
# Usage: scripts/create-nightly-tag.sh

SHORT_COMMIT=$(git rev-parse --short HEAD)

# Skip if a nightly tag already exists for this commit
if git tag -l "v*-nightly.*.${SHORT_COMMIT}" | grep -q .; then
  echo "Nightly tag already exists for commit ${SHORT_COMMIT}, skipping." >&2
  exit 2
fi

# Find the latest stable version tag (exclude prereleases)
LATEST_STABLE=$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude '*-*' 2>/dev/null)
if [ -z "$LATEST_STABLE" ]; then
  echo "::error::No stable version tag found" >&2
  exit 1
fi

# Bump patch version: v0.5.4 → v0.5.5
MAJOR=$(echo "$LATEST_STABLE" | sed 's/^v//' | cut -d. -f1)
MINOR=$(echo "$LATEST_STABLE" | sed 's/^v//' | cut -d. -f2)
PATCH=$(echo "$LATEST_STABLE" | sed 's/^v//' | cut -d. -f3)
NEXT_PATCH=$((PATCH + 1))
NIGHTLY_VERSION="v${MAJOR}.${MINOR}.${NEXT_PATCH}"

DATE=$(TZ=UTC0 date +%Y%m%d%H%M)
TAG="${NIGHTLY_VERSION}-nightly.${DATE}.${SHORT_COMMIT}"

# Skip if this exact tag already exists
if git rev-parse "${TAG}" >/dev/null 2>&1; then
  echo "Tag ${TAG} already exists, skipping." >&2
  exit 2
fi

echo "${TAG}"
