#!/bin/bash
#
# Publishes the channel pointer that install.sh and the CLI's update check read
# before they fall back to the GitHub API:
#
#   <bucket>/channels/stable.json
#   <bucket>/channels/nightly.json
#
# One request, one answer. That removes api.github.com from the version-lookup
# path entirely, and with it the unauthenticated 60-requests-per-hour-per-IP
# limit that a shared office or CI egress address can exhaust on its own.
#
# Run this AFTER the release artifacts are uploaded and verified. The pointer is
# what makes a version visible, so flipping it before the bytes are in place —
# or before the macOS signing check has passed — would advertise a release
# nobody can install.
#
# The manifest deliberately carries no checksums. checksums.txt, which
# GoReleaser generates and mirrors next to the archives, stays the single
# integrity source; two files listing the same hashes is two files that can
# disagree.
#
# Usage:
#   publish-channel-manifest.sh --tag vX.Y.Z --bucket NAME [--region R] [--force]
#
# Exit codes:
#   0  published (or nothing to do)
#   1  bad usage / upload failure
#   2  refused: the channel already points at a newer version (use --force)

set -euo pipefail

TAG=""
BUCKET=""
REGION="us-east-2"
FORCE=0

die() { printf 'Error: %s\n' "$1" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --tag)    shift; [[ $# -gt 0 ]] || die "--tag requires a value"; TAG="$1" ;;
        --bucket) shift; [[ $# -gt 0 ]] || die "--bucket requires a value"; BUCKET="$1" ;;
        --region) shift; [[ $# -gt 0 ]] || die "--region requires a value"; REGION="$1" ;;
        --force)  FORCE=1 ;;
        -h|--help)
            cat <<USAGE
Usage: publish-channel-manifest.sh --tag vX.Y.Z --bucket NAME [--region R] [--force]

Publishes the channel pointer (channels/stable.json or channels/nightly.json)
that install.sh and the CLI's update check read before falling back to the
GitHub API. The channel is derived from the tag: a prerelease segment means
nightly, anything else means stable.

Run this only after the release artifacts are uploaded and verified — the
pointer is what makes a version visible.

Options:
  --tag      Release tag to publish, with the v prefix (required)
  --bucket   Destination bucket (required)
  --region   AWS region (default: us-east-2)
  --force    Move the pointer backwards to an older version
  -h/--help  Show this help message

Exit codes:
  0  published, or already current
  1  bad usage, or the upload failed
  2  refused: the channel already points at a newer version
USAGE
            exit 0
            ;;
        *) die "Unknown argument: $1" ;;
    esac
    shift
done

[[ -n "$TAG" ]] || die "--tag is required"
[[ -n "$BUCKET" ]] || die "--bucket is required"
command -v aws >/dev/null 2>&1 || die "the aws CLI is required"

VERSION="${TAG#v}"
# No "+" in the prerelease: both readers reject build metadata (install.sh's
# regex has none, and versioncheck requires semver.Canonical(tag) == tag), so
# allowing it here publishes a pointer every reader ignores and leaves the
# channel silently stuck on the previous version.
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] \
    || die "tag '${TAG}' is not a semver release tag"

# A nightly prerelease belongs to nightly, no prerelease to stable. Any other
# prerelease is refused rather than routed: readers identify the nightly channel
# by the "-nightly." prerelease segment, so an rc tag sent to nightly would
# publish a pointer they reject, leaving nightly silently stuck on the previous
# version. Refusing here makes that a loud failure at publish time instead.
#
# The test is the literal "-nightly." segment, matching install.sh's
# version_valid_for_channel, versioncheck's isNightly and its GitHub release
# scan. A looser test here (any prerelease merely containing "nightly") routes
# v0.5.4-nightly or v0.5.4-prenightly to the channel, where the CLI accepts
# them and install.sh does not -- the CLI then announces an update that the
# installer refuses to install.
if [[ "$VERSION" == *-nightly.* ]]; then
    CHANNEL="nightly"
elif [[ "$VERSION" == *-* ]]; then
    # Not an error. The release itself is fine — GoReleaser has already
    # published it and the artifacts are mirrored; there is simply no channel
    # to advertise it on. Dying here would fail the release job after
    # publication, fire the Slack notification and skip mirror-nightly, so a
    # valid prerelease would look like a broken release. Said loudly on stderr
    # so a typo'd nightly tag is still visible in the release log.
    printf 'No channel claims %s: its prerelease segment is not a nightly, so no pointer was moved. The release itself is unaffected.\n' \
        "$TAG" >&2
    exit 0
else
    CHANNEL="stable"
fi

KEY="channels/${CHANNEL}.json"
S3_URI="s3://${BUCKET}/${KEY}"

# semver_gt A B -> true when A is strictly newer than B.
semver_gt() {
    python3 - "$1" "$2" <<'PY'
import sys

def parse(v):
    core, _, pre = v.partition("-")
    nums = tuple(int(p) for p in core.split("."))
    # An absent prerelease outranks any prerelease of the same core version.
    return nums, pre

def key(v):
    nums, pre = parse(v)
    if not pre:
        return (nums, 1, ())
    parts = tuple(
        (0, int(p), "") if p.isdigit() else (1, 0, p)
        for p in pre.split(".")
    )
    return (nums, 0, parts)

a, b = sys.argv[1], sys.argv[2]
sys.exit(0 if key(a) > key(b) else 1)
PY
}

# Monotonic guard. release.yml can be dispatched with any version string, so
# without this a re-run of an old tag would quietly point every fresh install
# and every update prompt at a version we already moved past.
#
# This guard fails CLOSED, which is deliberately the opposite of the read
# chains in versioncheck and install.sh. There an unreachable origin advances
# to the next candidate, because the caller still needs an answer and a wrong
# "you are up to date" is the worse outcome. Here the only question is "can I
# prove this move is forward", and a pointer we could not read cannot prove
# it. So only a definite not-found counts as "nothing to move past";
# permission denied, throttling and network errors abort, because treating
# them as absence is what lets a re-dispatch during a transient S3 error
# regress the channel for every install and update check.
CURRENT=""
CURRENT_ETAG=""
READ_ERR="$(mktemp)"
READ_BODY="$(mktemp)"
# get-object rather than `s3 cp`, because the write below is conditional on the
# ETag this read returns and `aws s3 cp` cannot surface it.
if READ_META="$(aws s3api get-object --bucket "$BUCKET" --key "$KEY" --region "$REGION" "$READ_BODY" 2>"$READ_ERR")"; then
    CURRENT_JSON="$(cat "$READ_BODY")"
    CURRENT_ETAG="$(printf '%s' "$READ_META" \
        | grep -o '"ETag"[^,}]*' \
        | sed -E 's/.*"ETag": *"?\\?"?([^"\\]+).*/\1/' \
        | head -n 1 || true)"
    # `|| true` so a manifest without a "version" key reaches the explicit
    # check below with an empty CURRENT, instead of grep's exit status
    # tripping `set -e` and aborting with a bare status and no explanation.
    CURRENT="$(printf '%s' "$CURRENT_JSON" \
        | grep '"version"' \
        | sed -E 's/.*"version": *"v?([^"]+)".*/\1/' \
        | head -n 1 || true)"
    if [[ -z "$CURRENT" ]]; then
        # A pointer that exists but names no version is not an empty channel:
        # something published a manifest we cannot parse, and overwriting it
        # unchecked would move the channel by an unknown amount.
        rm -f "$READ_ERR" "$READ_BODY"
        printf 'channels/%s.json exists but names no version. Refusing to publish over a pointer that cannot be read; inspect %s by hand.\n' \
            "$CHANNEL" "$S3_URI" >&2
        exit 2
    fi
elif grep -qiE '404|not found|nosuchkey|does not exist' "$READ_ERR"; then
    : # No pointer yet: this is the first publish on this channel.
else
    printf 'Could not read the current %s pointer, so this publish cannot be proven to move it forward. Refusing.\n' \
        "$CHANNEL" >&2
    sed 's/^/  /' "$READ_ERR" >&2
    rm -f "$READ_ERR" "$READ_BODY"
    exit 2
fi
rm -f "$READ_ERR" "$READ_BODY"

if [[ -n "$CURRENT" ]]; then
    if [[ "$CURRENT" == "$VERSION" ]]; then
        printf 'channels/%s.json already points at %s — nothing to do.\n' "$CHANNEL" "$VERSION"
        exit 0
    fi
    if ! semver_gt "$VERSION" "$CURRENT"; then
        if [[ "$FORCE" -ne 1 ]]; then
            printf 'Refusing to move the %s channel backwards: %s -> %s. Re-run with --force to override.\n' \
                "$CHANNEL" "$CURRENT" "$VERSION" >&2
            exit 2
        fi
        printf 'Moving the %s channel backwards (%s -> %s) because --force was given.\n' \
            "$CHANNEL" "$CURRENT" "$VERSION" >&2
    fi
fi

PUBLISHED_AT="$(TZ=UTC0 date +%Y-%m-%dT%H:%M:%SZ)"
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT
cat > "$TMP" <<JSON
{
  "channel": "${CHANNEL}",
  "tag": "${TAG}",
  "version": "${VERSION}",
  "published_at": "${PUBLISHED_AT}"
}
JSON

# Conditional on the exact object the guard above inspected. Reading the
# pointer, comparing versions and writing are three operations; between the
# read and the write another publisher can land a newer version, and both would
# pass their own guard. --if-match makes S3 reject the write if the object
# changed, and --if-none-match '*' makes a first publish refuse if someone
# created the pointer in the meantime.
#
# A conflict aborts rather than retrying. The guard's whole purpose is to not
# move the channel on an unproven comparison, and a retry would have to redo the
# comparison anyway — re-running the step is both simpler and more obvious in a
# release log.
#
# A short TTL, not an immutable one: this object is the thing that changes when
# a release ships. Readers check at most once every 24h, so a minute of edge
# caching costs nothing and still absorbs a stampede.
PUT_ERR="$(mktemp)"
if [[ -n "$CURRENT_ETAG" ]]; then
    CONDITION=(--if-match "$CURRENT_ETAG")
else
    CONDITION=(--if-none-match "*")
fi

if ! aws s3api put-object \
    --bucket "$BUCKET" \
    --key "$KEY" \
    --body "$TMP" \
    --region "$REGION" \
    --content-type application/json \
    --cache-control "public, max-age=60" \
    "${CONDITION[@]}" >/dev/null 2>"$PUT_ERR"; then
    if grep -qiE 'precondition|conditional request failed|412' "$PUT_ERR"; then
        printf 'The %s pointer changed while this publish was running, so the monotonic check no longer holds. Nothing was written — re-run to compare against the new value.\n' \
            "$CHANNEL" >&2
        sed 's/^/  /' "$PUT_ERR" >&2
        rm -f "$PUT_ERR"
        exit 2
    fi
    printf 'Publishing channels/%s.json failed.\n' "$CHANNEL" >&2
    sed 's/^/  /' "$PUT_ERR" >&2
    rm -f "$PUT_ERR"
    exit 1
fi
rm -f "$PUT_ERR"

printf 'Published %s -> %s (%s)\n' "$KEY" "$VERSION" "$CHANNEL"
