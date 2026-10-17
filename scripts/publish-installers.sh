#!/bin/bash
#
# Mirrors the install scripts themselves to the release origin:
#
#   <bucket>/installers/install.sh
#   <bucket>/installers/install.ps1
#
# Without this the binaries survive a GitHub outage but the one-liner does not:
# entire.com/install.{sh,ps1} redirects to raw.githubusercontent.com, so a new
# install still fails at hop zero. Mirroring the scripts removes that hop, and
# fixes a version-skew bug at the same time — the redirect serves main's
# installer, never the released one.
#
# This does NOT make every install path GitHub-free. install.ps1 is mirrored so
# it can be fetched, but it still resolves versions through api.github.com,
# downloads archives from GitHub releases, and adds a Scoop bucket by git URL.
# Windows therefore remains outage-affected; see "Remaining work" in
# docs/architecture/release-origins.md.
#
# Run this on STABLE releases only. These two keys are overwritten on every
# release rather than versioned, so a nightly publishing here would replace the
# installer that stable users fetch. Nightly users are served by the same
# stable installer, which takes --channel nightly.
#
# Run it BEFORE the channel pointer is flipped: the pointer is what makes a
# version visible, and an installer that is newer than the pointer is harmless
# while the reverse is not.
#
# Usage:
#   publish-installers.sh --bucket NAME [--region R]
#
# Exit codes:
#   0  published
#   1  bad usage / missing script / upload failure

set -euo pipefail

BUCKET=""
REGION="us-east-2"

die() { printf 'Error: %s\n' "$1" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --bucket) shift; [[ $# -gt 0 ]] || die "--bucket requires a value"; BUCKET="$1" ;;
        --region) shift; [[ $# -gt 0 ]] || die "--region requires a value"; REGION="$1" ;;
        -h|--help)
            cat <<USAGE
Usage: publish-installers.sh --bucket NAME [--region R]

Mirrors scripts/install.sh and scripts/install.ps1 to <bucket>/installers/ so
the install one-liner keeps working during a GitHub outage.

Stable releases only: these keys are overwritten rather than versioned, so a
nightly would replace the installer stable users fetch.

Options:
  --bucket   Destination bucket (required)
  --region   Bucket region (default: us-east-2)
USAGE
            exit 0 ;;
        *) die "unknown argument: $1" ;;
    esac
    shift
done

[[ -n "$BUCKET" ]] || die "--bucket is required"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# A short TTL, not the artifacts' immutable one: these keys are overwritten in
# place, so a long max-age would pin a stale installer in every CDN edge and
# every user's cache for the life of that TTL.
CACHE_CONTROL="public, max-age=300"

# Content types are set explicitly because the bucket is served through a
# Worker that passes them through; without them a browser is offered a
# download of application/octet-stream instead of readable text, and anyone
# auditing the script before piping it to a shell cannot simply open the URL.
publish_one() {
    local name="$1" content_type="$2"
    local src="$SCRIPT_DIR/$name"

    [[ -f "$src" ]] || die "$name not found at $src"

    aws s3 cp "$src" "s3://${BUCKET}/installers/${name}" \
        --region "$REGION" \
        --content-type "$content_type" \
        --cache-control "$CACHE_CONTROL" \
        || die "uploading $name failed"

    printf 'published installers/%s (%s)\n' "$name" "$content_type"
}

publish_one install.sh "text/x-shellscript"
publish_one install.ps1 "text/plain; charset=utf-8"
