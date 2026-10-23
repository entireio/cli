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
# It also mirrors the Scoop manifest, with its download URLs rewritten to this
# origin:
#
#   <bucket>/scoop/entire.json
#
# Scoop does not need a bucket, and therefore does not need git: `scoop install
# <url-to-manifest.json>` installs from a URL and records that URL as the app's
# source, so `scoop update` re-fetches it and upgrades in place. Both verified
# against Scoop v0.6.0. The bucket manifest is left pointing at GitHub, so the
# ordinary `scoop install entire` path is unchanged; this is an additional
# GitHub-free route, not a replacement that makes every Scoop install depend on
# one origin.
#
# install.ps1 itself is still not covered: it is mirrored so it can be fetched,
# but it resolves versions through api.github.com and downloads archives from
# GitHub releases. See "Remaining work" in
# docs/architecture/release-origins.md.
#
# Run this on STABLE releases only. Every key it writes is overwritten on each
# release rather than versioned, so a nightly publishing here would replace what
# stable users fetch. Nightly users are served by the same stable installer,
# which takes --channel nightly.
#
# Run it AFTER the channel pointer is flipped. These keys are shared across
# versions, so publishing them first means a release the pointer then refuses --
# an older tag re-dispatched -- leaves them rolled back with nothing advanced.
# The installer is version-independent, so a pointer that moved first is
# harmless; the reverse leaves a silent rollback behind a failed job.
#
# Usage:
#   publish-installers.sh --bucket NAME [--region R] [--dist DIR] [--origin-url URL]
#
# Exit codes:
#   0  published
#   1  bad usage / missing script / upload failure

set -euo pipefail

BUCKET=""
REGION="us-east-2"
DIST="dist"
# The public name this bucket is served under. The manifest has to name a URL a
# Scoop client can fetch, which the bucket name is not.
ORIGIN_URL="https://install.entire.com"

die() { printf 'Error: %s\n' "$1" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --bucket) shift; [[ $# -gt 0 ]] || die "--bucket requires a value"; BUCKET="$1" ;;
        --region) shift; [[ $# -gt 0 ]] || die "--region requires a value"; REGION="$1" ;;
        --dist)   shift; [[ $# -gt 0 ]] || die "--dist requires a value"; DIST="$1" ;;
        --origin-url) shift; [[ $# -gt 0 ]] || die "--origin-url requires a value"; ORIGIN_URL="$1" ;;
        -h|--help)
            cat <<USAGE
Usage: publish-installers.sh --bucket NAME [--region R] [--dist DIR] [--origin-url URL]

Mirrors scripts/install.sh and scripts/install.ps1 to <bucket>/installers/ so
the install one-liner keeps working during a GitHub outage.

Stable releases only: these keys are overwritten rather than versioned, so a
nightly would replace the installer stable users fetch.

Options:
  --bucket       Destination bucket (required)
  --region       Bucket region (default: us-east-2)
  --dist         GoReleaser output directory (default: dist)
  --origin-url   Public base URL this bucket is served under
                 (default: https://install.entire.com)
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

# The Scoop manifest, with its artifact URLs repointed at this origin. Scoop has
# no fallback of its own, so a manifest naming GitHub would leave the break-glass
# route depending on the thing it exists to route around.
GITHUB_DOWNLOAD_PREFIX="https://github.com/entireio/cli/releases/download/"
manifest_src="${DIST}/scoop/entire.json"
[[ -f "$manifest_src" ]] || die "Scoop manifest not found at ${manifest_src}; run this after GoReleaser"

manifest_tmp="$(mktemp)"
trap 'rm -f "$manifest_tmp"' EXIT
sed "s|${GITHUB_DOWNLOAD_PREFIX}|${ORIGIN_URL%/}/|g" "$manifest_src" > "$manifest_tmp"

# Fail loudly if the rewrite missed. A manifest that still names GitHub would
# publish and look fine, and only be discovered during the outage it is for.
if grep -q "$GITHUB_DOWNLOAD_PREFIX" "$manifest_tmp"; then
    die "the Scoop manifest still names GitHub after the rewrite; the URL format in ${manifest_src} has changed"
fi
if ! grep -q "${ORIGIN_URL%/}/" "$manifest_tmp"; then
    die "the Scoop manifest names no ${ORIGIN_URL} URL after the rewrite; nothing was repointed"
fi

aws s3 cp "$manifest_tmp" "s3://${BUCKET}/scoop/entire.json" \
    --region "$REGION" \
    --content-type application/json \
    --cache-control "$CACHE_CONTROL" \
    || die "uploading the Scoop manifest failed"

printf 'published scoop/entire.json (urls repointed at %s)\n' "$ORIGIN_URL"
