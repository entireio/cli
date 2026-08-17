#!/usr/bin/env bash
set -euo pipefail

# Calculate the dev release tag for HEAD.
#
# The dev channel tracks main: one release, replaced on every merge that passes
# tests, so `install.sh --channel dev` always fetches current main. Tags are
# created once and never moved — moving a published tag would make every plain
# `git fetch` in every clone report "would clobber existing tag", forever. The
# workflow deletes the superseded tag instead.
#
# Tag format: v<major>.<minor>.<patch+1>-dev.<YYYYMMDDhhmm>.<short-commit>
# The -dev. prerelease suffix matters beyond naming: release.yml keys on it to
# publish as a prerelease, and goreleaser stamps experimental.Visible=true for
# any prerelease, so dogfooders keep seeing experimental commands.
#
# Exit codes: 0 with the tag on stdout, 2 if HEAD already has one (nothing to
# do), 1 on error.
#
# Deliberately parallel to create-nightly-tag.sh rather than shared with it: that
# script feeds the externally-consumed nightly channel, and this change is not the
# place to refactor a published release path. Keep the two in step.
# Usage: scripts/create-dev-tag.sh

SHORT_COMMIT=$(git rev-parse --short HEAD)

# Skip if a dev tag already exists for this commit. Makes the workflow safe to
# re-run, and to fire twice for one commit (a re-run of Tests, a manual dispatch).
if git tag -l "v*-dev.*.${SHORT_COMMIT}" | grep -q .; then
  echo "Dev tag already exists for commit ${SHORT_COMMIT}, skipping." >&2
  exit 2
fi

# Base the version on the latest stable tag, so a dev build sorts above the
# current release and below the next one.
LATEST_STABLE=$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude '*-*' 2>/dev/null)
if [ -z "$LATEST_STABLE" ]; then
  echo "::error::No stable version tag found" >&2
  exit 1
fi

MAJOR=$(echo "$LATEST_STABLE" | sed 's/^v//' | cut -d. -f1)
MINOR=$(echo "$LATEST_STABLE" | sed 's/^v//' | cut -d. -f2)
PATCH=$(echo "$LATEST_STABLE" | sed 's/^v//' | cut -d. -f3)
NEXT_PATCH=$((PATCH + 1))
DEV_VERSION="v${MAJOR}.${MINOR}.${NEXT_PATCH}"

DATE=$(TZ=UTC0 date +%Y%m%d%H%M)
TAG="${DEV_VERSION}-dev.${DATE}.${SHORT_COMMIT}"

# Two merges inside the same minute would otherwise collide on the timestamp.
if git rev-parse "${TAG}" >/dev/null 2>&1; then
  echo "Tag ${TAG} already exists, skipping." >&2
  exit 2
fi

echo "${TAG}"
