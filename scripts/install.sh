#!/bin/bash

set -euo pipefail

GITHUB_REPO="entireio/cli"
BINARY_NAME="entire"
DEFAULT_INSTALL_DIR="$HOME/.local/bin"

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

show_help() {
    cat << EOF
Entire CLI Installer

Usage:
    curl -fsSL https://entire.io/install.sh | bash

Environment Variables:
    ENTIRE_INSTALL_DIR  Override install directory (default: $HOME/.local/bin)
    GITHUB_TOKEN        GitHub API token to avoid rate limiting (optional)

Examples:
    # Install latest version
    curl -fsSL https://entire.io/install.sh | bash

    # Install to custom directory
    curl -fsSL https://entire.io/install.sh | ENTIRE_INSTALL_DIR=/usr/local/bin bash
EOF
    exit 0
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

get_latest_version() {
    local url="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
    local version
    local curl_opts=(-fsSL)
    if [[ -n "${GITHUB_TOKEN:-}" ]]; then
        curl_opts+=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
    fi
    version=$(curl "${curl_opts[@]}" "$url" 2>/dev/null | grep '"tag_name"' | sed -E 's/.*"tag_name": *"v?([^"]+)".*/\1/')

    if [[ -z "$version" ]]; then
        error "Failed to fetch latest version from GitHub. Please check your internet connection."
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
    for arg in "$@"; do
        if [[ "$arg" == "--help" || "$arg" == "-h" ]]; then
            show_help
        fi
    done

    if ! command -v curl &> /dev/null; then
        error "curl is required but not installed. Please install curl and try again."
    fi

    info "Installing Entire CLI..."

    # Detect platform
    local os arch
    os=$(detect_os)
    arch=$(detect_arch)
    info "Detected platform: ${os}/${arch}"

    info "Fetching latest version..."
    local version
    version=$(get_latest_version)
    # Strip leading 'v' if present
    version="${version#v}"
    info "Installing version: ${version}"

    # Construct download URL
    local archive_name="${BINARY_NAME}_${os}_${arch}.tar.gz"
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

    local install_dir="${ENTIRE_INSTALL_DIR:-$DEFAULT_INSTALL_DIR}"
    local binary_path="${tmp_dir}/${BINARY_NAME}"

    chmod +x "$binary_path"

    info "Installing to ${install_dir}..."
    local install_path="${install_dir}/${BINARY_NAME}"

    mkdir -p "${install_dir}"

    if [[ ! -w "$install_dir" ]]; then
        error "Cannot write to ${install_dir}. Run with sudo or set ENTIRE_INSTALL_DIR to a writable location."
    fi
    mv "$binary_path" "$install_path"

    # Verify installation
    if "$install_path" version &> /dev/null; then
        success "Entire CLI installed successfully!"
    else
        error "Installation completed but the binary failed to execute. Please check the installation."
    fi

    # Check if the installed binary is the one that will be found in PATH
    local path_binary
    path_binary=$(command -v "$BINARY_NAME" 2>/dev/null || true)
    if [[ -n "$path_binary" && "$path_binary" != "$install_path" ]]; then
        echo ""
        echo -e "${YELLOW}!${NC} ${BOLD}WARNING: PATH conflict detected${NC}"
        echo -e "${YELLOW}!${NC}"
        echo -e "${YELLOW}!${NC} Installed to: ${install_path}"
        echo -e "${YELLOW}!${NC} But '${BINARY_NAME}' resolves to: ${path_binary}"
        echo -e "${YELLOW}!${NC}"
        echo -e "${YELLOW}!${NC} Your PATH may have another version earlier. To fix:"
        echo -e "${YELLOW}!${NC}   1. Remove the old binary: rm ${path_binary}"
        echo -e "${YELLOW}!${NC}   2. Or adjust your PATH to prioritize ${install_dir}"
        echo ""
    elif [[ -z "$path_binary" ]]; then
        echo ""
        echo -e "${YELLOW}!${NC} ${BOLD}WARNING: '${BINARY_NAME}' not found in PATH${NC}"
        echo -e "${YELLOW}!${NC}"
        echo -e "${YELLOW}!${NC} Installed to: ${install_path}"
        echo -e "${YELLOW}!${NC} But this directory is not in your PATH."
        echo -e "${YELLOW}!${NC}"
        echo -e "${YELLOW}!${NC} Add to your shell config (.bashrc, .zshrc, etc.):"
        echo -e "${YELLOW}!${NC}   export PATH=\"\$PATH:${install_dir}\""
        echo ""
    fi

    info "Running post-install actions..."
    "$install_path" curl-bash-post-install
}

main "$@"
