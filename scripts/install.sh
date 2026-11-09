#!/bin/bash

set -euo pipefail

GITHUB_REPO="entireio/cli"
DEFAULT_INSTALL_DIR="$HOME/.local/bin"
DEFAULT_CHANNEL="stable"

# Release origins, in the order they are tried.
#
# Ours comes first. Reading it first is what makes an install work while GitHub
# is down, and — because every install uses it, not just installs during an
# incident — it also means the path cannot rot into a fallback nobody has
# exercised. GitHub stays as the automatic second choice, so a problem with our
# own origin degrades to the previous behaviour instead of breaking installs.
#
# Both origins lay artifacts out the same way, <base>/v<version>/<file>, so one
# URL builder covers both.
DEFAULT_RELEASE_ORIGIN="https://install.entire.com"
GITHUB_RELEASE_ORIGIN="https://github.com/${GITHUB_REPO}/releases/download"
GITHUB_API="https://api.github.com/repos/${GITHUB_REPO}"

# Colors (disabled in non-interactive mode)
if [[ -t 1 ]]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[0;33m'
    BLUE='\033[0;34m'
    BOLD='\033[1m'
    NC='\033[0m' # No Color
else
    RED=''
    GREEN=''
    YELLOW=''
    BLUE=''
    BOLD=''
    NC=''
fi

info() {
    printf '%b%s%b\n' "${BLUE}==>${NC} ${BOLD}" "$1" "${NC}"
}

success() {
    printf '%b%s%b\n' "${GREEN}==>${NC} ${BOLD}" "$1" "${NC}"
}

warn() {
    printf '%b %s\n' "${YELLOW}Warning:${NC}" "$1"
}

error() {
    printf '%b %s\n' "${RED}Error:${NC}" "$1" >&2
    exit 1
}

usage() {
    cat <<EOF
Usage: install.sh [--channel stable|nightly] [--version X.Y.Z]

Options:
  --channel   Release channel to install (default: stable)
  --version   Install this exact version instead of the channel's latest
  -h, --help  Show this help message

Environment:
  ENTIRE_INSTALL_BASE_URL  Use only this release origin, skipping the GitHub
                           fallback. Set it to pin an install to one mirror.
  GITHUB_TOKEN             Sent to the GitHub API if the fallback is reached.
EOF
}

detect_os() {
    local os
    os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    case "$os" in
        darwin)
            echo "darwin"
            ;;
        linux)
            echo "linux"
            ;;
        mingw*|msys*|cygwin*)
            error "install.sh does not support Windows. Run this from PowerShell 5.1 or later:

    irm https://entire.com/install.ps1 -UseBasicParsing | iex

  Or install the Entire CLI using Scoop:

    scoop install entire/entire

  Or download the Windows zip from:
    https://github.com/entireio/cli/releases/latest"
            ;;
        *)
            error "Unsupported operating system: $os"
            ;;
    esac
}

detect_arch() {
    local arch
    arch="$(uname -m)"
    case "$arch" in
        x86_64|amd64)
            echo "amd64"
            ;;
        arm64|aarch64)
            echo "arm64"
            ;;
        *)
            error "Unsupported architecture: $arch"
            ;;
    esac
}

# Timeouts exist so a reachable-but-broken origin fails over instead of hanging.
# Metadata requests are small and get a hard ceiling; archives only get a connect
# timeout, because capping total time would kill a working download on a slow
# link — the failure we are guarding against is a dead origin, not a slow one.
CURL_METADATA_OPTS=(--connect-timeout 5 --max-time 15)
# --connect-timeout only bounds connection setup. An origin that accepts the
# connection and then sends nothing would otherwise stall the install forever
# and never reach the fallback. --speed-limit/--speed-time bounds that without
# capping a slow-but-progressing download the way --max-time would: abort only
# if throughput stays under 1KB/s for 30s. Verified to fire before any headers
# arrive, which is the case that matters here.
CURL_DOWNLOAD_OPTS=(--connect-timeout 10 --speed-limit 1024 --speed-time 30)

fetch_github_json() {
    local url="$1"
    local curl_opts=(-fsSL "${CURL_METADATA_OPTS[@]}")
    if [[ -n "${GITHUB_TOKEN:-}" ]]; then
        curl_opts+=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
    fi

    curl "${curl_opts[@]}" "$url" 2>/dev/null
}

# release_origins prints the origins to try, in order. An explicit
# ENTIRE_INSTALL_BASE_URL is authoritative: someone pinning a mirror wants that
# mirror, not a silent fall back to GitHub that hides whether the mirror worked.
release_origins() {
    if [[ -n "${ENTIRE_INSTALL_BASE_URL:-}" ]]; then
        echo "entire"
        return
    fi
    echo "entire"
    echo "github"
}

# origin_artifact_base prints the base URL under which <base>/v<version>/<file>
# resolves for the given origin.
origin_artifact_base() {
    case "$1" in
        entire) printf '%s' "${ENTIRE_INSTALL_BASE_URL:-$DEFAULT_RELEASE_ORIGIN}" ;;
        github) printf '%s' "$GITHUB_RELEASE_ORIGIN" ;;
        *)      return 1 ;;
    esac
}

# origin_label prints a short human name, used in progress and error output so
# a failed install says which origin was being read.
origin_label() {
    case "$1" in
        entire) printf '%s' "${ENTIRE_INSTALL_BASE_URL:-$DEFAULT_RELEASE_ORIGIN}" ;;
        github) printf '%s' "github.com/${GITHUB_REPO}" ;;
        *)      return 1 ;;
    esac
}

# version_valid_for_channel rejects a manifest value that is not a version, or
# that belongs to the other channel. Without this a value like "not-a-version"
# ends discovery here, and both origins are then asked for a release that does
# not exist — failing the install even when GitHub has a valid one. Invalid
# metadata has to advance to the fallback, not stop the walk.
# version_is_release_version checks the shape alone. Split out because every
# version that reaches a URL has to pass it, including one pinned with
# --version, where there is no channel to check against. It is interpolated
# into ${base}/v${version}/..., and curl normalises ".." segments, so an
# unchecked value can address something outside the v<version>/ prefix
# entirely and fail later as a generic download error.
version_is_release_version() {
    printf '%s' "$1" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
}

version_valid_for_channel() {
    local version="$1" channel="$2"

    if ! version_is_release_version "$version"; then
        return 1
    fi
    if [[ "$channel" == "nightly" ]]; then
        if ! printf '%s' "$version" | grep -qE -- '-nightly\.'; then
            return 1
        fi
        return 0
    fi
    # Stable carries no prerelease segment; a nightly here would downgrade or
    # cross-grade every stable install.
    if printf '%s' "$version" | grep -qE -- '-'; then
        return 1
    fi
    return 0
}

# manifest_field prints one string field from a channel manifest, or nothing.
manifest_field() {
    printf '%s' "$1" \
        | grep -o "\"$2\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" \
        | sed -E 's/.*: *"([^"]*)".*/\1/' \
        | head -n 1 || true
}

# resolve_version_from_entire reads the channel pointer we publish at release
# time. One request, one answer — no list-and-filter, and no GitHub API rate
# limit (60/hour per IP unauthenticated, which a shared office or CI egress IP
# can exhaust on its own).
resolve_version_from_entire() {
    local channel="$1"
    local base body declared version
    base="$(origin_artifact_base entire)"
    body="$(curl -fsSL "${CURL_METADATA_OPTS[@]}" "${base}/channels/${channel}.json" 2>/dev/null || true)"
    [[ -n "$body" ]] || return 1

    # A manifest published to the wrong key, or left stale across a rename,
    # would otherwise cross the streams. The CLI's update check makes the same
    # check; both readers have to agree or the two disagree about what is
    # installable.
    declared="$(manifest_field "$body" channel)"
    if [[ -n "$declared" && "$declared" != "$channel" ]]; then
        return 1
    fi

    # tag first, then version, matching the CLI. They normally agree; when they
    # do not, the tag is the thing the artifacts were published under.
    version="$(manifest_field "$body" tag)"
    [[ -n "$version" ]] || version="$(manifest_field "$body" version)"
    version="${version#v}"

    if [[ -n "$version" ]] && version_valid_for_channel "$version" "$channel"; then
        printf '%s' "$version"
        return 0
    fi
    return 1
}

resolve_version_from_github() {
    local channel="$1" version
    if [[ "$channel" == "nightly" ]]; then
        # The literal -nightly. segment, not a bare "nightly" substring: a
        # looser match here selects tags every other reader rejects, so the two
        # origins in this script would disagree about what is installable.
        version="$(fetch_github_json "${GITHUB_API}/releases?per_page=20" \
            | grep '"tag_name"' \
            | grep -- '-nightly\.' \
            | head -n 1 \
            | sed -E 's/.*"tag_name": *"v?([^"]+)".*/\1/' || true)"
    else
        version="$(fetch_github_json "${GITHUB_API}/releases/latest" \
            | grep '"tag_name"' \
            | sed -E 's/.*"tag_name": *"v?([^"]+)".*/\1/' || true)"
    fi

    # Validated exactly like the release origin's answer. An origin we do not
    # own is not more trustworthy than one we do.
    if [[ -n "$version" ]] && version_valid_for_channel "$version" "$channel"; then
        printf '%s' "$version"
        return 0
    fi
    return 1
}

resolve_version_from_origin() {
    case "$1" in
        entire) resolve_version_from_entire "$2" ;;
        github) resolve_version_from_github "$2" ;;
        *)      return 1 ;;
    esac
}

# resolve_version walks the origins and prints the first version any of them
# names. Failing to reach one origin is not fatal; failing to reach all of them
# is.
resolve_version() {
    local channel="$1"
    local origin version
    for origin in $(release_origins); do
        version="$(resolve_version_from_origin "$origin" "$channel" || true)"
        if [[ -n "$version" ]]; then
            printf '%s' "$version"
            return 0
        fi
    done
    return 1
}

download_file() {
    local url="$1"
    local output="$2"
    local curl_opts=(-fsSL "${CURL_DOWNLOAD_OPTS[@]}")

    curl "${curl_opts[@]}" "$url" -o "$output"
}

verify_checksum() {
    local file="$1"
    local expected_checksum="$2"
    local actual_checksum

    if command -v sha256sum &> /dev/null; then
        actual_checksum=$(sha256sum "$file" | awk '{print $1}')
    elif command -v shasum &> /dev/null; then
        actual_checksum=$(shasum -a 256 "$file" | awk '{print $1}')
    else
        warn "No checksum tool found (sha256sum or shasum). Skipping verification."
        return 0
    fi

    if [[ "$actual_checksum" != "$expected_checksum" ]]; then
        error "Checksum verification failed!  Expected: $expected_checksum, actual: $actual_checksum"
    fi
}

# fetch_release_from_origin downloads the archive and checksums.txt for one
# version from a single origin and verifies one against the other.
#
# Archive and checksums always come from the SAME origin. Mixing them would let
# a healthy origin's checksums vouch for another origin's bytes, which is a
# weaker guarantee than either origin offers on its own.
#
# Return codes drive the caller's chain, and the split follows the same rule as
# the checkpoint read chain: a definite bad answer aborts, an unreachable one
# advances.
#   0  archive downloaded and verified
#   1  this origin could not serve the release — try the next one
# A checksum MISMATCH does not return: it calls error and exits. Bytes that
# disagree with their own origin's checksums mean that origin is corrupt or
# hostile, and quietly retrying elsewhere would hide it.
fetch_release_from_origin() {
    local origin="$1" version="$2" archive_name="$3" tmp_dir="$4"
    local base label archive_path checksums_path expected_checksum
    base="$(origin_artifact_base "$origin")"
    label="$(origin_label "$origin")"
    archive_path="${tmp_dir}/${archive_name}"
    checksums_path="${tmp_dir}/checksums.txt"

    info "Downloading ${archive_name} from ${label}..."
    if ! download_file "${base}/v${version}/${archive_name}" "$archive_path"; then
        warn "Could not download ${archive_name} from ${label}"
        return 1
    fi

    if ! download_file "${base}/v${version}/checksums.txt" "$checksums_path"; then
        warn "Could not download checksums.txt from ${label}"
        return 1
    fi

    expected_checksum=$(grep -iE "${archive_name}\$" "$checksums_path" | awk '{print $1}' || true)
    if [[ -z "$expected_checksum" ]]; then
        warn "checksums.txt from ${label} does not list ${archive_name}"
        return 1
    fi

    info "Verifying checksum..."
    verify_checksum "$archive_path" "$expected_checksum"
    success "Checksum verified (${label})"
    return 0
}

main() {
    local channel="${DEFAULT_CHANNEL}"
    local channel_given=0
    local version=""

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --channel)
                shift
                [[ $# -gt 0 ]] || error "--channel requires a value"
                channel="$1"
                channel_given=1
                ;;
            --version)
                shift
                [[ $# -gt 0 ]] || error "--version requires a value"
                version="${1#v}"
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            *)
                error "Unknown argument: $1"
                ;;
        esac
        shift
    done

    if ! command -v curl &> /dev/null; then
        error "curl is required but not installed. Please install curl and try again."
    fi

    case "$channel" in
        stable|nightly) ;;
        *)
            error "Unsupported channel: ${channel}. Expected 'stable' or 'nightly'."
            ;;
    esac

    # --version pins a specific build and skips discovery, so it cannot also
    # honour --channel. Silently installing a stable build under
    # `--channel nightly --version X` is the worst outcome; refusing is better
    # than guessing which half of a contradictory request was meant.
    if [[ -n "$version" ]]; then
        if ! version_is_release_version "$version"; then
            error "--version ${version} is not a version. Expected X.Y.Z, optionally with a prerelease suffix."
        fi
        if [[ "$channel_given" -eq 1 ]] && ! version_valid_for_channel "$version" "$channel"; then
            error "--version ${version} is not a ${channel} version. Drop --channel to install it, or pick a ${channel} version."
        fi
    fi

    info "Installing Entire CLI..."

    # Detect platform
    local os arch
    os=$(detect_os)
    arch=$(detect_arch)
    info "Detected platform: ${os}/${arch}"

    if [[ -z "$version" ]]; then
        info "Fetching latest ${channel} version..."
        version="$(resolve_version "$channel" || true)"
        if [[ -z "$version" ]]; then
            error "Could not determine the latest ${channel} version from any release origin.
  Tried: $(for o in $(release_origins); do printf '%s ' "$(origin_label "$o")"; done)
  Check your internet connection, or install a known version directly:
    curl -fsSL https://entire.com/install.sh | bash -s -- --version X.Y.Z"
        fi
    fi
    # Strip leading 'v' if present
    version="${version#v}"
    info "Installing version: ${version}"

    local archive_name="entire_${os}_${arch}.tar.gz"

    tmp_dir=$(mktemp -d)
    trap 'rm -rf "$tmp_dir"' EXIT

    local origin fetched=0
    for origin in $(release_origins); do
        if fetch_release_from_origin "$origin" "$version" "$archive_name" "$tmp_dir"; then
            fetched=1
            break
        fi
    done
    if [[ "$fetched" -ne 1 ]]; then
        error "Could not download Entire CLI v${version} from any release origin.
  Tried: $(for o in $(release_origins); do printf '%s ' "$(origin_label "$o")"; done)
  Check that the version exists and try again."
    fi

    local archive_path="${tmp_dir}/${archive_name}"

    info "Extracting..."
    tar -xzf "$archive_path" -C "$tmp_dir"

    local install_dir="${DEFAULT_INSTALL_DIR}"
    local binary_path="${tmp_dir}/entire"

    chmod +x "$binary_path"

    info "Installing to ${install_dir}..."
    local install_path="${install_dir}/entire"

    mkdir -p "${install_dir}"
    info "Directory ready"

    if [[ ! -w "$install_dir" ]]; then
        error "Cannot write to ${install_dir}."
    fi
    mv "$binary_path" "$install_path"

    # Install the git remote helper binary alongside entire so
    # `git clone entire://…` resolves it on PATH. It ships in the same
    # archive as a separate binary.
    if [[ -f "${tmp_dir}/git-remote-entire" ]]; then
        chmod +x "${tmp_dir}/git-remote-entire"
        mv "${tmp_dir}/git-remote-entire" "${install_dir}/git-remote-entire"
    else
        warn "git-remote-entire not found in archive; entire:// clones won't work until the next release includes it."
    fi

    # Verify installation
    if "$install_path" version &> /dev/null; then
        success "Entire CLI installed to ${install_path}"
    else
        error "Installation completed but the binary failed to execute. Please check the installation."
    fi

    # Check if the installed binary is the one that will be found in PATH
    local path_binary
    path_binary=$(command -v "entire" 2>/dev/null || true)
    if [[ -n "$path_binary" && ! "$path_binary" -ef "$install_path" ]]; then
        # This case is a bit weird, because some other 'entire' is found on PATH.  Warn user.
        echo ""
        echo -e "${YELLOW}!${NC} ${BOLD}WARNING: PATH conflict detected${NC}"
        echo -e "${YELLOW}!${NC}"
        echo -e "${YELLOW}!${NC} Installed to: ${install_path}"
        echo -e "${YELLOW}!${NC} But 'entire' resolves to: ${path_binary}"
        echo -e "${YELLOW}!${NC}"
        echo -e "${YELLOW}!${NC} Your PATH may have another version of Entire CLI. To fix:"
        echo -e "${YELLOW}!${NC}   1. Remove the old binary: rm ${path_binary}"
        echo -e "${YELLOW}!${NC}   or"
        echo -e "${YELLOW}!${NC}   2. Adjust your PATH to prioritize ${install_dir}"
        echo ""
        error "Installation completed but PATH needs adjustment. Then, rerun the installation."
    elif [[ -z "$path_binary" ]]; then
        # First-time install: ~/.local/bin likely isn't on their PATH yet.
        # Detect their shell and show the right config file.
        local shell_name shell_config
        shell_name="$(basename "${SHELL:-}")"
        case "$shell_name" in
            zsh)
                # shellcheck disable=SC2088
                shell_config="~/.zshrc" ;;
            bash)
                if [[ -f "$HOME/.bash_profile" ]]; then
                    # shellcheck disable=SC2088
                    shell_config="~/.bash_profile"
                else
                    # shellcheck disable=SC2088
                    shell_config="~/.bashrc"
                fi
                ;;
            fish)
                # shellcheck disable=SC2088
                shell_config="~/.config/fish/config.fish" ;;
            *)
                shell_config="" ;;
        esac

        echo ""
        echo -e "  Add ${BOLD}entire${NC} to your PATH:"
        echo ""
        if [[ "$shell_name" == "fish" ]]; then
            echo -e "    ${BOLD}mkdir -p ~/.config/fish${NC}"
            echo -e "    ${BOLD}echo 'fish_add_path ${install_dir}' >> \$HOME/.config/fish/config.fish${NC}"
        elif [[ -n "$shell_config" ]]; then
            echo -e "    ${BOLD}echo 'export PATH=\"${install_dir}:\$PATH\"' >> ${shell_config}${NC}"
        else
            echo -e "  Add this to your shell config:"
            echo ""
            echo -e "    ${BOLD}export PATH=\"${install_dir}:\$PATH\"${NC}"
            echo ""
            echo -e "  Restart your terminal, then run ${BOLD}entire${NC} to get started."
            exit 0
        fi
        echo ""
        echo -e "  Restart your terminal, then run ${BOLD}entire${NC} to get started."
        exit 0
    fi

    info "Running post-install actions..."
    "$install_path" curl-bash-post-install
}

main "$@"
