#!/bin/bash
#
# Exercises the two-origin release path against local fake origins. No network,
# no writes outside a temp dir.
#
# Why this exists: install.sh reads our own release origin first and falls back
# to GitHub. The fallback only runs when something is broken, which is exactly
# when nobody wants to discover it never worked — so it is tested here instead
# of in production. Covers:
#
#   * install.sh resolving a version from channels/<channel>.json
#   * install.sh downloading and verifying from our origin
#   * install.sh falling back to the GitHub origin for both steps
#   * a checksum mismatch aborting instead of silently trying the next origin
#   * --version pinning, and the error text when every origin is unreachable
#   * publish-channel-manifest.sh routing, monotonicity and idempotence
#
# Requires: python3, curl, tar, sha256sum or shasum.
# Usage: scripts/test-release-origins.sh

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
INSTALL_SH="${REPO_ROOT}/scripts/install.sh"
PUBLISH_SH="${REPO_ROOT}/scripts/publish-channel-manifest.sh"
WORK="$(mktemp -d)"
# A PATH without the developer's real `entire`, so install.sh takes its
# first-time-install branch instead of the PATH-conflict branch.
SAFE_PATH="/usr/bin:/bin:/usr/sbin:/sbin"

pass=0
fail=0
ok()   { printf '  \033[0;32mPASS\033[0m %s\n' "$1"; pass=$((pass + 1)); }
nope() { printf '  \033[0;31mFAIL\033[0m %s\n' "$1"; fail=$((fail + 1)); }
section() { printf '\n\033[1m%s\033[0m\n' "$1"; }
indent() { while IFS= read -r line; do printf '      %s\n' "$line"; done; }

# expect <description> <command...> — passes when the command succeeds.
expect() {
    local desc="$1"
    shift
    if "$@"; then ok "$desc"; else nope "$desc"; fi
}

# refute <description> <command...> — passes when the command fails.
refute() {
    local desc="$1"
    shift
    if "$@"; then nope "$desc"; else ok "$desc"; fi
}

contains() { grep -qF -- "$2" <<< "$1"; }
eq()       { [ "$1" = "$2" ]; }

cleanup() {
    for pidfile in "$WORK"/*/.pid; do
        [ -f "$pidfile" ] && kill "$(cat "$pidfile")" 2>/dev/null
    done
    rm -rf "$WORK"
}
trap cleanup EXIT

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1"
    else
        shasum -a 256 "$1"
    fi
}

archive_name() {
    local os arch
    os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    arch="$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"
    printf 'entire_%s_%s.tar.gz' "$os" "$arch"
}

# build_origin <dir> <version> — a release tree shaped like both real origins:
# <root>/v<version>/{archive,checksums.txt} plus <root>/channels/*.json
build_origin() {
    local dir="$1" version="$2" arch
    arch="$(archive_name)"
    mkdir -p "$dir/v$version" "$dir/channels" "$dir/.stage"
    printf '#!/bin/sh\necho "entire %s"\n' "$version" > "$dir/.stage/entire"
    printf '#!/bin/sh\necho helper\n' > "$dir/.stage/git-remote-entire"
    chmod +x "$dir/.stage/entire" "$dir/.stage/git-remote-entire"
    tar -czf "$dir/v$version/$arch" -C "$dir/.stage" entire git-remote-entire
    ( cd "$dir/v$version" && sha256_of "$arch" > checksums.txt )
    printf '{"channel":"stable","tag":"v%s","version":"%s"}\n' "$version" "$version" \
        > "$dir/channels/stable.json"
}

# blackhole <dir> -> prints a port that accepts connections and never responds.
# A dead origin fails fast on connect; this is the nastier case — reachable, so
# curl waits. install.sh must still fail over.
blackhole() {
    local dir="$1" port
    mkdir -p "$dir"
    port="$(free_port)"
    python3 -c '
import socket, sys, time
srv = socket.socket()
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", int(sys.argv[1])))
srv.listen(16)
held = []
while True:
    conn, _ = srv.accept()
    held.append(conn)  # accepted, deliberately never answered
    time.sleep(0.01)
' "$port" >/dev/null 2>&1 &
    echo $! > "$dir/.pid"
    echo "$port"
}

free_port() {
    python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'
}

serve() { # <dir> -> prints the port it is listening on
    local dir="$1" port
    port="$(free_port)"
    ( cd "$dir" && exec python3 -m http.server "$port" --bind 127.0.0.1 >/dev/null 2>&1 ) &
    echo $! > "$dir/.pid"
    for _ in $(seq 1 50); do
        curl -fsS -o /dev/null "http://127.0.0.1:$port/" 2>/dev/null && break
        sleep 0.1
    done
    echo "$port"
}

# repoint <out> <our-origin> <github-origin> <github-api>
# A copy of install.sh with its origin constants aimed at local servers, so the
# two-tier chain runs without touching the network.
repoint() {
    sed -e "s|^DEFAULT_RELEASE_ORIGIN=.*|DEFAULT_RELEASE_ORIGIN=\"$2\"|" \
        -e "s|^GITHUB_RELEASE_ORIGIN=.*|GITHUB_RELEASE_ORIGIN=\"$3\"|" \
        -e "s|^GITHUB_API=.*|GITHUB_API=\"$4\"|" \
        "$INSTALL_SH" > "$1"
}

# run_install <home> <script> [args...]
#
# Runs install.sh with an empty environment and an isolated HOME, so it can
# never write to the developer's real ~/.local/bin. Set BASE_URL to point it at
# a local origin.
#
# env -i wipes the environment, so BASE_URL has to be re-injected explicitly —
# exporting it around the call is not enough. Getting that wrong made two cases
# silently download the real CLI from the real GitHub, so the guard below
# refuses to run a script that is still aimed at a public origin.
run_install() {
    local home="$1" script="$2"
    shift 2
    mkdir -p "$home"

    local -a env_args=(HOME="$home" PATH="$SAFE_PATH" SHELL=/bin/bash)
    if [ -n "${BASE_URL:-}" ]; then
        env_args+=("ENTIRE_INSTALL_BASE_URL=$BASE_URL")
    elif ! grep -q '^DEFAULT_RELEASE_ORIGIN="http://127.0.0.1:' "$script"; then
        printf 'Error: %s would reach a public origin. Set BASE_URL or use repoint().\n' \
            "$(basename "$script")" >&2
        return 1
    fi

    env -i "${env_args[@]}" bash "$script" "$@" 2>&1
}

for tool in python3 curl tar; do
    command -v "$tool" >/dev/null 2>&1 || { printf 'Error: %s is required\n' "$tool" >&2; exit 1; }
done

# ============================================================ install.sh
section "install.sh — our origin first"

build_origin "$WORK/ours" 9.9.9
OURS_PORT="$(serve "$WORK/ours")"
OURS="http://127.0.0.1:$OURS_PORT"
ARCH="$(archive_name)"

out="$(BASE_URL="$OURS" run_install "$WORK/home-ours" "$INSTALL_SH")"
expect "resolves the version from channels/stable.json" contains "$out" "Installing version: 9.9.9"
expect "verifies the archive against our origin's checksums.txt" \
    contains "$out" "Checksum verified ($OURS)"
expect "installs entire" test -x "$WORK/home-ours/.local/bin/entire"
expect "installs git-remote-entire" test -x "$WORK/home-ours/.local/bin/git-remote-entire"
refute "does not touch GitHub when our origin answers" contains "$out" "github"

section "install.sh — falls back to GitHub"

build_origin "$WORK/gh" 8.8.8
mkdir -p "$WORK/gh/api/releases"
printf '{"tag_name":"v8.8.8","prerelease":false}\n' > "$WORK/gh/api/releases/latest"
GH_PORT="$(serve "$WORK/gh")"
GH="http://127.0.0.1:$GH_PORT"
DEAD="http://127.0.0.1:$(free_port)"

repoint "$WORK/install-fallback.sh" "$DEAD" "$GH" "$GH/api"
out="$(run_install "$WORK/home-fallback" "$WORK/install-fallback.sh")"
expect "falls back for version discovery" contains "$out" "Installing version: 8.8.8"
expect "falls back for the download" contains "$out" "Checksum verified (github.com/"
expect "the fallback install completes" test -x "$WORK/home-fallback/.local/bin/entire"

section "install.sh — a checksum mismatch aborts"

build_origin "$WORK/bad" 7.7.7
printf '%s  %s\n' "$(printf '0%.0s' $(seq 1 64))" "$ARCH" > "$WORK/bad/v7.7.7/checksums.txt"
BAD_PORT="$(serve "$WORK/bad")"
repoint "$WORK/install-badsum.sh" "http://127.0.0.1:$BAD_PORT" "$GH" "$GH/api"
out="$(run_install "$WORK/home-badsum" "$WORK/install-badsum.sh")"
rc=$?
refute "exits non-zero" eq "$rc" 0
expect "says the checksum failed" contains "$out" "Checksum verification failed"
refute "installs nothing" test -e "$WORK/home-badsum/.local/bin/entire"
refute "does not fall back after a mismatch" contains "$out" "Checksum verified"

section "install.sh — pinning and unreachable origins"

out="$(BASE_URL="$OURS" run_install "$WORK/home-pin" "$INSTALL_SH" --version v9.9.9)"
refute "--version skips version discovery" contains "$out" "Fetching latest"
expect "--version installs" test -x "$WORK/home-pin/.local/bin/entire"

repoint "$WORK/install-alldown.sh" "$DEAD" "$DEAD" "$DEAD/api"
out="$(run_install "$WORK/home-alldown" "$WORK/install-alldown.sh")"
rc=$?
refute "every origin down exits non-zero" eq "$rc" 0
expect "the error names the pinning escape hatch" contains "$out" "--version X.Y.Z"

section "install.sh — a hanging origin still fails over"

# Shortens the real policy rather than substituting a different one. The
# download path has no --max-time in production — injecting one here would make
# this pass on a bound that does not exist, which is exactly the failure it is
# supposed to catch. So the metadata budget shrinks (it already has --max-time)
# and the download keeps --speed-limit/--speed-time with a 2s window.
BH_PORT="$(blackhole "$WORK/blackhole")"
repoint "$WORK/install-hang.sh" "http://127.0.0.1:$BH_PORT" "$GH" "$GH/api"
sed -i.bak \
    -e 's|^CURL_METADATA_OPTS=.*|CURL_METADATA_OPTS=(--connect-timeout 1 --max-time 2)|' \
    -e 's|^CURL_DOWNLOAD_OPTS=.*|CURL_DOWNLOAD_OPTS=(--connect-timeout 1 --speed-limit 1 --speed-time 2)|' \
    "$WORK/install-hang.sh"
started="$(date +%s)"
out="$(run_install "$WORK/home-hang" "$WORK/install-hang.sh")"
elapsed=$(( $(date +%s) - started ))
expect "falls over to GitHub instead of hanging" contains "$out" "Installing version: 8.8.8"
expect "the install completes despite the hung origin" test -x "$WORK/home-hang/.local/bin/entire"
expect "gives up on the hung origin within the budget" test "$elapsed" -lt 20

# ============================================================ channel pointer
section "install.sh — the GitHub fallback is held to the same rules"

# The fallback used a bare "nightly" substring and validated nothing, so the two
# origins in this script disagreed about what counts as installable.
build_origin "$WORK/ghloose" 5.5.5
mkdir -p "$WORK/ghloose/api"
# Pretty-printed, one "tag_name" per line, as the real API returns it. A
# single-line fixture never exercises the per-line filter this guards.
cat > "$WORK/ghloose/api/releases" <<'JSON'
[
  {
    "tag_name": "v4.4.4-prenightly",
    "prerelease": true
  },
  {
    "tag_name": "v3.3.3-nightly.202604061200.abc1234",
    "prerelease": true
  }
]
JSON
GHLOOSE_PORT="$(serve "$WORK/ghloose")"
GHL="http://127.0.0.1:$GHLOOSE_PORT"
DEADL="http://127.0.0.1:$(free_port)"
build_origin "$WORK/ghloose" 3.3.3-nightly.202604061200.abc1234
repoint "$WORK/install-ghloose.sh" "$DEADL" "$GHL" "$GHL/api"
out="$(run_install "$WORK/home-ghloose" "$WORK/install-ghloose.sh" --channel nightly)"
expect "a near-miss nightly tag is skipped on the fallback too" \
    contains "$out" "Installing version: 3.3.3-nightly.202604061200.abc1234"
refute "the near-miss tag is never selected" contains "$out" "prenightly"

section "install.sh — --version is validated on its own"

# It is interpolated into ${base}/v${version}/..., and curl normalises ".."
# segments, so an unchecked value addresses something outside the version
# prefix and fails later as a generic download error.
repoint "$WORK/install-badver.sh" "$DEAD" "$GH" "$GH/api"
for bad in "../../y" "not-a-version" "1.2"; do
    out="$(run_install "$WORK/home-badver" "$WORK/install-badver.sh" --version "$bad"; printf 'rc=%s' "$?")"
    refute "--version $bad does not reach a download" contains "$out" "Installing version"
    expect "--version $bad is rejected as not a version" contains "$out" "is not a version"
done

section "install.sh — --version and --channel cannot contradict"

repoint "$WORK/install-conflict.sh" "$DEAD" "$GH" "$GH/api"
out="$(run_install "$WORK/home-conflict" "$WORK/install-conflict.sh" --channel nightly --version 0.11.0; printf 'rc=%s' "$?")"
refute "a stable version under --channel nightly is not installed" contains "$out" "Installing version: 0.11.0"
expect "it says the version is not of that channel" contains "$out" "is not a nightly version"

section "install.sh — unusable manifest metadata falls back"

# A version that is not a version must not end discovery. Both origins would
# then be asked for a release that does not exist, failing the install even
# though GitHub has a valid one.
build_origin "$WORK/badmeta" 5.5.5
printf '{"channel":"stable","tag":"vnot-a-version","version":"not-a-version"}\n' \
    > "$WORK/badmeta/channels/stable.json"
BADMETA_PORT="$(serve "$WORK/badmeta")"
repoint "$WORK/install-badmeta.sh" "http://127.0.0.1:$BADMETA_PORT" "$GH" "$GH/api"
out="$(run_install "$WORK/home-badmeta" "$WORK/install-badmeta.sh")"
expect "a manifest naming a non-version falls back to GitHub" contains "$out" "Installing version: 8.8.8"
expect "the install completes from the fallback" test -x "$WORK/home-badmeta/.local/bin/entire"
refute "the bad version is never requested" contains "$out" "not-a-version"

# A nightly on the stable channel is the cross-stream case: accepting it would
# move every stable install onto a prerelease.
build_origin "$WORK/crossmeta" 6.6.6
printf '{"channel":"stable","version":"4.4.4-nightly.202604061200.abc1234"}\n' \
    > "$WORK/crossmeta/channels/stable.json"
CROSS_PORT="$(serve "$WORK/crossmeta")"
repoint "$WORK/install-crossmeta.sh" "http://127.0.0.1:$CROSS_PORT" "$GH" "$GH/api"
out="$(run_install "$WORK/home-crossmeta" "$WORK/install-crossmeta.sh")"
expect "a nightly on the stable channel falls back to GitHub" contains "$out" "Installing version: 8.8.8"
refute "the nightly version is never installed" contains "$out" "4.4.4-nightly"

# The CLI reads tag before version and checks the declared channel; install.sh
# has to agree or the two disagree about what is installable.
build_origin "$WORK/tagpref" 3.3.3
printf '{"channel":"stable","tag":"v3.3.3","version":"9.9.9"}\n' \
    > "$WORK/tagpref/channels/stable.json"
TAGPREF_PORT="$(serve "$WORK/tagpref")"
repoint "$WORK/install-tagpref.sh" "http://127.0.0.1:$TAGPREF_PORT" "$GH" "$GH/api"
out="$(run_install "$WORK/home-tagpref" "$WORK/install-tagpref.sh")"
expect "tag wins over version, as in the CLI" contains "$out" "Installing version: 3.3.3"

build_origin "$WORK/wrongchan" 2.2.2
printf '{"channel":"nightly","tag":"v2.2.2"}\n' > "$WORK/wrongchan/channels/stable.json"
WRONGCHAN_PORT="$(serve "$WORK/wrongchan")"
repoint "$WORK/install-wrongchan.sh" "http://127.0.0.1:$WRONGCHAN_PORT" "$GH" "$GH/api"
out="$(run_install "$WORK/home-wrongchan" "$WORK/install-wrongchan.sh")"
expect "a manifest declaring another channel falls back" contains "$out" "Installing version: 8.8.8"

section "the release mirror stays inert when the variable is empty"

# release.yml always exports ENTIRE_RELEASE_BUCKET, and an unset Actions
# variable exports as an empty string, so isEnvSet alone is true on every
# release. Gating on presence enables the blobs pipe with an empty bucket and
# no credentials, failing the release instead of staying inert.
expect "the blobs gate tests non-emptiness, not just presence" \
    grep -qF 'ne .Env.ENTIRE_RELEASE_BUCKET ""' "$REPO_ROOT/.goreleaser.yaml"
refute "no bare isEnvSet gate remains on the bucket" \
    grep -qE "if isEnvSet \"ENTIRE_RELEASE_BUCKET\"" "$REPO_ROOT/.goreleaser.yaml"
refute "no bare isEnvSet gate remains on the region" \
    grep -qE "if isEnvSet \"ENTIRE_RELEASE_REGION\"" "$REPO_ROOT/.goreleaser.yaml"

section "publish-channel-manifest.sh"

STUB_BIN="$WORK/stub-bin"
mkdir -p "$STUB_BIN"
cat > "$STUB_BIN/aws" <<'AWS'
#!/bin/bash
# Stub for the AWS CLI surface these scripts use, mapping s3://bucket/key and
# --bucket/--key to $AWS_STUB_ROOT/bucket/key.
#
#   s3 cp                 (publish-installers.sh)
#   s3api get-object      (publish-channel-manifest.sh, needs the ETag)
#   s3api put-object      (publish-channel-manifest.sh, conditional write)
#
# ETags are the content md5, as S3 does for a single-part upload, so --if-match
# behaves the way the real conditional write does.
set -uo pipefail
echo "$*" >> "$AWS_STUB_ROOT/calls.log"

s3path() { printf '%s/%s' "$AWS_STUB_ROOT" "${1#s3://}"; }
etag_of() { md5 -q "$1" 2>/dev/null || md5sum "$1" | cut -d' ' -f1; }
not_found() {
    echo "fatal error: An error occurred (404) when calling the HeadObject operation: Not Found" >&2
    exit 1
}

if [ "${1:-}" = "s3" ] && [ "${2:-}" = "cp" ]; then
    src="$3"; dst="$4"
    if [ -n "${AWS_STUB_READ_ERROR:-}" ] && [ "${src#s3://}" != "$src" ]; then
        echo "fatal error: An error occurred (AccessDenied) when calling the HeadObject operation: Forbidden" >&2
        exit 1
    fi
    if [ "${src#s3://}" != "$src" ]; then
        f="$(s3path "$src")"; [ -f "$f" ] || not_found; cat "$f"; exit 0
    fi
    f="$(s3path "$dst")"; mkdir -p "$(dirname "$f")"; cp "$src" "$f"; exit 0
fi

if [ "${1:-}" = "s3api" ]; then
    op="$2"; shift 2
    bucket=""; key=""; body=""; outfile=""; if_match=""; if_none_match=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --bucket)         bucket="$2"; shift 2 ;;
            --key)            key="$2"; shift 2 ;;
            --body)           body="$2"; shift 2 ;;
            --if-match)       if_match="$2"; shift 2 ;;
            --if-none-match)  if_none_match="$2"; shift 2 ;;
            --region|--content-type|--cache-control) shift 2 ;;
            --*)              shift ;;
            *)                outfile="$1"; shift ;;
        esac
    done
    f="$AWS_STUB_ROOT/$bucket/$key"

    case "$op" in
        get-object)
            [ -z "${AWS_STUB_READ_ERROR:-}" ] || {
                echo "fatal error: An error occurred (AccessDenied) when calling the GetObject operation: Forbidden" >&2
                exit 1
            }
            [ -f "$f" ] || not_found
            [ -n "$outfile" ] && cp "$f" "$outfile"
            # AWS_STUB_STALE_ETAG reports an ETag that will not match at write
            # time, which is what a concurrent publisher looks like.
            if [ -n "${AWS_STUB_STALE_ETAG:-}" ]; then
                printf '{"ETag": "\"%s\"", "ContentType": "application/json"}\n' "stale00000000000000000000000000"
                exit 0
            fi
            printf '{"ETag": "\"%s\"", "ContentType": "application/json"}\n' "$(etag_of "$f")"
            exit 0 ;;
        put-object)
            if [ -n "$if_none_match" ] && [ -f "$f" ]; then
                echo "An error occurred (PreconditionFailed) when calling the PutObject operation: At least one of the pre-conditions you specified did not hold" >&2
                exit 1
            fi
            if [ -n "$if_match" ]; then
                [ -f "$f" ] || not_found
                if [ "$if_match" != "$(etag_of "$f")" ]; then
                    echo "An error occurred (PreconditionFailed) when calling the PutObject operation: At least one of the pre-conditions you specified did not hold" >&2
                    exit 1
                fi
            fi
            mkdir -p "$(dirname "$f")"; cp "$body" "$f"; exit 0 ;;
        *) exit 64 ;;
    esac
fi

exit 64
AWS
chmod +x "$STUB_BIN/aws"

publish() { PATH="$STUB_BIN:$SAFE_PATH" bash "$PUBLISH_SH" "$@" 2>&1; }
fresh() { export AWS_STUB_ROOT="$WORK/s3-$1"; rm -rf "$AWS_STUB_ROOT"; mkdir -p "$AWS_STUB_ROOT/b"; }
seed() {
    mkdir -p "$AWS_STUB_ROOT/b/channels"
    printf '{"channel":"%s","tag":"v%s","version":"%s"}\n' "$1" "$2" "$2" \
        > "$AWS_STUB_ROOT/b/channels/$1.json"
}
pointer() {
    grep '"version"' "$AWS_STUB_ROOT/b/channels/$1.json" 2>/dev/null \
        | sed -E 's/.*"version": *"([^"]+)".*/\1/'
}

fresh cold
publish --tag v1.2.3 --bucket b >/dev/null
expect "a stable tag writes channels/stable.json" eq "$(pointer stable)" "1.2.3"
expect "the pointer gets a short TTL, not an immutable one" \
    grep -qF "max-age=60" "$AWS_STUB_ROOT/calls.log"

fresh nightly
publish --tag v1.2.4-nightly.202608180612.abc1234 --bucket b >/dev/null
expect "a nightly tag writes channels/nightly.json" \
    eq "$(pointer nightly)" "1.2.4-nightly.202608180612.abc1234"
refute "a nightly never touches the stable pointer" test -f "$AWS_STUB_ROOT/b/channels/stable.json"

fresh forward
seed stable 1.2.3
publish --tag v1.3.0 --bucket b >/dev/null
expect "moves the pointer forward" eq "$(pointer stable)" "1.3.0"

fresh backward
seed stable 1.3.0
out="$(publish --tag v1.2.3 --bucket b)"; rc=$?
expect "refuses to move the pointer backwards" eq "$rc" 2
expect "the pointer is unchanged after a refusal" eq "$(pointer stable)" "1.3.0"
expect "the refusal names the override" contains "$out" "--force"

fresh forced
seed stable 1.3.0
publish --tag v1.2.3 --bucket b --force >/dev/null
expect "--force moves it backwards" eq "$(pointer stable)" "1.2.3"

fresh repeat
seed stable 1.2.3
out="$(publish --tag v1.2.3 --bucket b)"; rc=$?
expect "re-publishing the same version exits 0" eq "$rc" 0
expect "re-publishing the same version is a no-op" contains "$out" "nothing to do"

fresh older-nightly
seed nightly 1.2.4-nightly.202608180612.bbb1111
out="$(publish --tag v1.2.4-nightly.202608170612.aaa0000 --bucket b)"; rc=$?
expect "refuses an older nightly timestamp" eq "$rc" 2

fresh prerelease-vs-core
seed nightly 1.2.3
out="$(publish --tag v1.2.3-nightly.202608180612.abc1234 --bucket b)"; rc=$?
expect "a prerelease ranks below the released core version" eq "$rc" 2

fresh badinput
publish --tag not-a-version --bucket b >/dev/null 2>&1; rc=$?
expect "rejects a tag that is not semver" eq "$rc" 1
publish --bucket b >/dev/null 2>&1; rc=$?
# A prerelease no channel claims is "nothing to publish", not a failed release:
# GoReleaser has already published by this point, so dying would fail the job
# after the fact, fire Slack and skip mirror-nightly.
fresh rctag
out="$(publish --tag v1.2.3-rc.1 --bucket b; printf 'rc=%s' "$?")"
expect "an unclaimed prerelease is not a failure" contains "$out" "rc=0"
expect "it says no channel claims the tag" contains "$out" "No channel claims"
refute "no stable pointer is written" test -f "$AWS_STUB_ROOT/b/channels/stable.json"
refute "no nightly pointer is written" test -f "$AWS_STUB_ROOT/b/channels/nightly.json"

# A tag that is not a version at all stays a hard error — that is a broken
# release, not an unclaimed one.
fresh junktag
out="$(publish --tag vnot-a-version --bucket b; printf 'rc=%s' "$?")"
expect "a non-semver tag still fails" contains "$out" "rc=1"

# The publisher's nightly test must be the readers' test, not a looser one. A
# prerelease merely containing "nightly" would be routed to the channel, where
# the CLI accepts it and install.sh does not.
for t in v0.5.4-nightly v0.5.4-prenightly v0.5.4-rc.nightly.1; do
    fresh "nearmiss-$t"
    out="$(publish --tag "$t" --bucket b; printf 'rc=%s' "$?")"
    refute "$t never reaches the nightly pointer" test -f "$AWS_STUB_ROOT/b/channels/nightly.json"
    expect "$t is reported as unclaimed" contains "$out" "No channel claims"
done

fresh realnightly
out="$(publish --tag v0.5.4-nightly.202604061200.abc1234 --bucket b; printf 'rc=%s' "$?")"
expect "a real nightly tag still publishes" contains "$out" "rc=0"
expect "it lands on the nightly channel" eq "$(pointer nightly)" "0.5.4-nightly.202604061200.abc1234"

expect "requires --tag" eq "$rc" 1

# The monotonic guard must fail closed: a pointer it could not read cannot be
# proven to be behind the version being published.
fresh readerr
seed stable 9.9.9
out="$(AWS_STUB_READ_ERROR=1 publish --tag v1.0.0 --bucket b; printf 'rc=%s' "$?")"
expect "a non-404 read error refuses to publish" contains "$out" "rc=2"
expect "the refusal says the move cannot be proven forward" contains "$out" "cannot be proven to move it forward"
expect "the pointer is untouched after a read error" eq "$(pointer stable)" "9.9.9"
refute "a read error does not fall through to a backwards publish" contains "$out" "Refusing to move"

# A manifest that exists but names no version is not an empty channel.
fresh unparseable
mkdir -p "$AWS_STUB_ROOT/b/channels"
printf '{"channel":"stable"}\n' > "$AWS_STUB_ROOT/b/channels/stable.json"
out="$(publish --tag v1.0.0 --bucket b; printf 'rc=%s' "$?")"
expect "an unparseable pointer refuses to publish" contains "$out" "rc=2"
expect "the refusal names the object to inspect" contains "$out" "names no version"

# The guard reads, compares and writes as three operations. A concurrent
# publisher landing in between would otherwise pass both guards and leave the
# pointer wherever the slower write finished.
fresh conflict
seed stable 1.0.0
out="$(AWS_STUB_STALE_ETAG=1 publish --tag v2.0.0 --bucket b; printf 'rc=%s' "$?")"
expect "a pointer that changed mid-publish is refused" contains "$out" "rc=2"
expect "the refusal says the comparison no longer holds" contains "$out" "no longer holds"
expect "the pointer is untouched after a conflict" eq "$(pointer stable)" "1.0.0"

fresh conditional
publish --tag v1.0.0 --bucket b >/dev/null
expect "a first publish is conditional on the key being absent" \
    grep -qF -- "--if-none-match" "$AWS_STUB_ROOT/calls.log"
publish --tag v1.1.0 --bucket b >/dev/null
expect "an update is conditional on the ETag it read" \
    grep -qF -- "--if-match" "$AWS_STUB_ROOT/calls.log"
expect "the conditional update still moved the pointer" eq "$(pointer stable)" "1.1.0"

printf '\n\033[1m%s\033[0m\n' "publish-installers.sh"

installers() { PATH="$STUB_BIN:$SAFE_PATH" bash "$REPO_ROOT/scripts/publish-installers.sh" "$@" 2>&1; }

fresh installers
mkdir -p "$WORK/dist0/scoop"
cat > "$WORK/dist0/scoop/entire.json" <<'JSON'
{"version":"1.2.3","url":"https://github.com/entireio/cli/releases/download/v1.2.3/entire_windows_amd64.zip","hash":"bbbb"}
JSON
out="$(installers --bucket b --dist "$WORK/dist0"; printf 'rc=%s' "$?")"
expect "publishes both installers" contains "$out" "rc=0"
expect "install.sh lands under installers/" \
    [ -f "$AWS_STUB_ROOT/b/installers/install.sh" ]
expect "install.ps1 lands under installers/" \
    [ -f "$AWS_STUB_ROOT/b/installers/install.ps1" ]
expect "the uploaded install.sh is the one in the repo" \
    cmp -s "$REPO_ROOT/scripts/install.sh" "$AWS_STUB_ROOT/b/installers/install.sh"

# Overwritten in place on every release, so a long TTL would pin a stale
# installer in every edge cache for the life of that TTL.
expect "installers get a short TTL, not the artifacts' immutable one" \
    grep -qF "max-age=300" "$AWS_STUB_ROOT/calls.log"
refute "installers are not marked immutable" \
    grep -qF "immutable" "$AWS_STUB_ROOT/calls.log"

# Served through the Worker as-is: without an explicit type the script is
# offered as an octet-stream download instead of text anyone can read before
# piping it to a shell.
expect "install.sh is published as a shell script" \
    grep -qF "text/x-shellscript" "$AWS_STUB_ROOT/calls.log"
expect "install.ps1 is published as readable text" \
    grep -qF "text/plain; charset=utf-8" "$AWS_STUB_ROOT/calls.log"

# Scoop installs from a manifest URL with no bucket and therefore no git, and
# records that URL as the app's source so `scoop update` re-fetches it. Verified
# against Scoop v0.6.0 on Windows. The mirrored manifest has to name this origin
# for its artifacts, because Scoop has no fallback of its own.
fresh scoopmanifest
mkdir -p "$WORK/dist/scoop"
cat > "$WORK/dist/scoop/entire.json" <<'JSON'
{
  "version": "1.2.3",
  "architecture": {
    "64bit": {
      "url": "https://github.com/entireio/cli/releases/download/v1.2.3/entire_windows_amd64.zip",
      "hash": "aaaa"
    }
  }
}
JSON
out="$(installers --bucket b --dist "$WORK/dist" --origin-url https://example.test; printf 'rc=%s' "$?")"
expect "publishes the scoop manifest" contains "$out" "rc=0"
expect "it lands at scoop/entire.json" test -f "$AWS_STUB_ROOT/b/scoop/entire.json"
expect "artifact urls are repointed at the origin" \
    grep -qF "https://example.test/v1.2.3/entire_windows_amd64.zip" "$AWS_STUB_ROOT/b/scoop/entire.json"
refute "no GitHub download url survives" \
    grep -qF "github.com/entireio/cli/releases/download" "$AWS_STUB_ROOT/b/scoop/entire.json"
expect "the hash is left untouched" \
    grep -qF '"hash": "aaaa"' "$AWS_STUB_ROOT/b/scoop/entire.json"
expect "it is served as json" grep -qF "application/json" "$AWS_STUB_ROOT/calls.log"

# A silently un-rewritten manifest would publish fine and only fail during the
# outage it exists for, so the rewrite asserts it changed something.
fresh scoopunchanged
mkdir -p "$WORK/dist2/scoop"
printf '{"version":"1.2.3","url":"https://cdn.example.org/x.zip"}\n' > "$WORK/dist2/scoop/entire.json"
out="$(installers --bucket b --dist "$WORK/dist2" --origin-url https://example.test; printf 'rc=%s' "$?")"
expect "a manifest nothing was repointed in is refused" contains "$out" "rc=1"
expect "the refusal says nothing was repointed" contains "$out" "nothing was repointed"

fresh scoopmissing
out="$(installers --bucket b --dist "$WORK/nope"; printf 'rc=%s' "$?")"
expect "a missing scoop manifest is an error, not a silent skip" contains "$out" "rc=1"
expect "the error names the path" contains "$out" "Scoop manifest not found"

out="$(installers; printf 'rc=%s' "$?")"
expect "requires --bucket" contains "$out" "rc=1"
expect "the usage error names the missing flag" contains "$out" "--bucket is required"

printf '\n  %d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
