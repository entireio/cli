#!/bin/bash

# This installer requires Bash. It can be launched from Fish, Zsh, or Bash with:
#   curl -fsSL https://entire.io/install.sh | bash

set -euo pipefail

GITHUB_REPO="entireio/cli"
DEFAULT_INSTALL_DIR="$HOME/.local/bin"
DEFAULT_CHANNEL="stable"

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
Usage: install.sh [--channel stable|nightly]

Options:
  --channel   Release channel to install (default: stable)
  -h, --help  Show this help message
EOF
}

normalize_shell_name() {
    local shell_name="$1"

    # ps implementations may pad `comm` output. Trim it before extracting the
    # executable name so the exact-match below still recognizes the shell.
    shell_name="${shell_name#"${shell_name%%[![:space:]]*}"}"
    shell_name="${shell_name%"${shell_name##*[![:space:]]}"}"
    shell_name="${shell_name##*/}"
    shell_name="${shell_name#-}"

    case "$shell_name" in
        bash|fish|zsh)
            echo "$shell_name"
            ;;
    esac
}

detect_user_shell() {
    local parent_command=""
    local shell_name=""

    # The installer itself runs under Bash, so inspect its parent first to find
    # the shell the user actually launched it from. $SHELL is only a fallback:
    # it commonly names the login shell rather than the current shell.
    if command -v ps &> /dev/null; then
        parent_command="$(ps -p "$PPID" -o comm= 2>/dev/null || true)"
        shell_name="$(normalize_shell_name "$parent_command")"
    fi

    if [[ -z "$shell_name" ]]; then
        shell_name="$(normalize_shell_name "${SHELL:-}")"
    fi

    echo "$shell_name"
}

show_path_setup() {
    local shell_name="$1"
    local install_dir="$2"
    local install_dir_display="$install_dir"
    local shell_config=""

    # Keep copy-paste instructions portable when installing under the user's
    # home directory, while still honoring any other install directory.
    if [[ "$install_dir" == "$HOME" ]]; then
        install_dir_display="\$HOME"
    elif [[ "$install_dir" == "$HOME/"* ]]; then
        install_dir_display="\$HOME/${install_dir#"$HOME/"}"
    fi

    echo ""
    echo -e "  Add ${BOLD}entire${NC} to your PATH:"
    echo ""

    case "$shell_name" in
        fish)
            # fish_add_path updates this Fish session and persists the path for
            # future sessions, so no config-file edit or restart is required.
            echo -e "    ${BOLD}fish_add_path \"${install_dir_display}\"${NC}"
            echo ""
            echo -e "  Then run ${BOLD}entire${NC} to get started."
            ;;
        zsh)
            # shellcheck disable=SC2088
            shell_config="~/.zshrc"
            echo -e "    ${BOLD}echo 'export PATH=\"${install_dir_display}:\$PATH\"' >> ${shell_config}${NC}"
            echo ""
            echo -e "  Restart your terminal, then run ${BOLD}entire${NC} to get started."
            ;;
        bash)
            if [[ -f "$HOME/.bash_profile" ]]; then
                # shellcheck disable=SC2088
                shell_config="~/.bash_profile"
            else
                # shellcheck disable=SC2088
                shell_config="~/.bashrc"
            fi
            echo -e "    ${BOLD}echo 'export PATH=\"${install_dir_display}:\$PATH\"' >> ${shell_config}${NC}"
            echo ""
            echo -e "  Restart your terminal, then run ${BOLD}entire${NC} to get started."
            ;;
        *)
            echo "  Fish:"
            echo -e "    ${BOLD}fish_add_path \"${install_dir_display}\"${NC}"
            echo ""
            echo "  Bash, Zsh, and other POSIX-compatible shells:"
            echo -e "    ${BOLD}export PATH=\"${install_dir_display}:\$PATH\"${NC}"
            echo ""
            echo -e "  Then run ${BOLD}entire${NC} to get started."
            ;;
    esac
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

    irm https://entire.io/install.ps1 -UseBasicParsing | iex

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

fetch_github_json() {
    local url="$1"
    local curl_opts=(-fsSL)
    if [[ -n "${GITHUB_TOKEN:-}" ]]; then
        curl_opts+=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
    fi

    curl "${curl_opts[@]}" "$url" 2>/dev/null
}

get_latest_stable_version() {
    local url="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
    local version
    version=$(fetch_github_json "$url" | grep '"tag_name"' | sed -E 's/.*"tag_name": *"v?([^"]+)".*/\1/')

    if [[ -z "$version" ]]; then
        error "Failed to fetch latest version from GitHub. Please check your internet connection."
    fi

    echo "$version"
}

get_latest_nightly_version() {
    local url="https://api.github.com/repos/${GITHUB_REPO}/releases?per_page=20"
    local version
    version=$(fetch_github_json "$url" | grep '"tag_name"' | grep 'nightly' | head -n 1 | sed -E 's/.*"tag_name": *"v?([^"]+)".*/\1/')

    if [[ -z "$version" ]]; then
        error "Failed to fetch latest nightly version from GitHub. Please check your internet connection."
    fi

    echo "$version"
}

download_file() {
    local url="$1"
    local output="$2"
    local curl_opts=(-fsSL)

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

main() {
    local channel="${DEFAULT_CHANNEL}"
    local version=""

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --channel)
                shift
                [[ $# -gt 0 ]] || error "--channel requires a value"
                channel="$1"
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

    info "Installing Entire CLI..."

    # Detect platform
    local os arch
    os=$(detect_os)
    arch=$(detect_arch)
    info "Detected platform: ${os}/${arch}"

    info "Fetching latest ${channel} version..."
    if [[ "$channel" == "nightly" ]]; then
        version=$(get_latest_nightly_version)
    else
        version=$(get_latest_stable_version)
    fi
    # Strip leading 'v' if present
    version="${version#v}"
    info "Installing version: ${version}"

    # Construct download URL
    local archive_name="entire_${os}_${arch}.tar.gz"
    local download_url="https://github.com/${GITHUB_REPO}/releases/download/v${version}/${archive_name}"
    local checksums_url="https://github.com/${GITHUB_REPO}/releases/download/v${version}/checksums.txt"

    tmp_dir=$(mktemp -d)
    trap 'rm -rf "$tmp_dir"' EXIT

    # Download archive
    local archive_path="${tmp_dir}/${archive_name}"
    info "Downloading ${archive_name}..."
    if ! download_file "$download_url" "$archive_path"; then
        error "Failed to download from ${download_url}. Please check that the version exists and try again."
    fi

    # Download and verify checksums
    info "Downloading checksums..."
    local checksums_path="${tmp_dir}/checksums.txt"
    if ! download_file "$checksums_url" "$checksums_path"; then
        error "Failed to download checksums from ${checksums_url}"
    fi

    info "Verifying checksum..."
    local expected_checksum
    expected_checksum=$(grep -iE "${archive_name}\$" "$checksums_path" | awk '{print $1}' || true)
    if [[ -z "$expected_checksum" ]]; then
        error "Checksum for ${archive_name} not found in checksums.txt"
    fi
    verify_checksum "$archive_path" "$expected_checksum"
    success "Checksum verified"

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

    # Detect the invoking shell before checking PATH. This process is Bash even
    # when the user launched it from Fish or Zsh.
    local shell_name
    shell_name="$(detect_user_shell)"

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
    fi

    # Use the absolute binary path so first-time installs can run post-install
    # actions before ~/.local/bin has been added to PATH. Tell post-install
    # where the binary lives so any completion command it writes can use that
    # directory as a PATH fallback until the user's shell setup is complete.
    local installer_path_dir=""
    if [[ -z "$path_binary" ]]; then
        installer_path_dir="$install_dir"
    fi
    local post_install_shell="${shell_name:-${SHELL:-}}"
    info "Running post-install actions..."
    # Released binaries that predate ENTIRE_INSTALLER_SHELL only inspect
    # SHELL. Override it for this child process so the updated installer still
    # targets the invoking shell while a compatible binary is rolling out.
    SHELL="$post_install_shell" \
        ENTIRE_INSTALLER_SHELL="$shell_name" \
        ENTIRE_INSTALLER_PATH_DIR="$installer_path_dir" \
        "$install_path" curl-bash-post-install

    if [[ -z "$path_binary" ]]; then
        # First-time install: ~/.local/bin likely isn't on their PATH yet.
        show_path_setup "$shell_name" "$install_dir"
        exit 0
    fi
}

if [[ -z "${BASH_SOURCE[0]:-}" || "${BASH_SOURCE[0]:-}" == "$0" ]]; then
    main "$@"
fi
