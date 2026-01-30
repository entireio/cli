#!/bin/bash
# Entire CLI installer
# Usage: curl -fsSL https://entire.io/install.sh | bash
#
# Environment variables:
#   ENTIRE_VERSION    - Install a specific version (e.g., "1.0.0")
#   ENTIRE_INSTALL_DIR - Override install directory

set -euo pipefail

GITHUB_REPO="entireio/cli"
BINARY_NAME="entire"
DEFAULT_INSTALL_DIR="/usr/local/bin"

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
    echo -e "${BLUE}==>${NC} ${BOLD}$1${NC}"
}

success() {
    echo -e "${GREEN}==>${NC} ${BOLD}$1${NC}"
}

warn() {
    echo -e "${YELLOW}Warning:${NC} $1"
}

error() {
    echo -e "${RED}Error:${NC} $1" >&2
    exit 1
}

show_help() {
    cat << EOF
Entire CLI Installer

Usage:
    curl -fsSL https://entire.io/install.sh | bash
    curl -fsSL https://entire.io/install.sh | ENTIRE_VERSION=1.0.0 bash

Environment Variables:
    ENTIRE_VERSION      Install a specific version (default: latest)
    ENTIRE_INSTALL_DIR  Override install directory (default: /usr/local/bin)

Examples:
    # Install latest version
    curl -fsSL https://entire.io/install.sh | bash

    # Install specific version
    curl -fsSL https://entire.io/install.sh | ENTIRE_VERSION=1.0.0 bash

    # Install to custom directory
    curl -fsSL https://entire.io/install.sh | ENTIRE_INSTALL_DIR=~/.local/bin bash
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
    version=$(curl -fsSL "$url" 2>/dev/null | grep '"tag_name"' | sed -E 's/.*"tag_name": *"v?([^"]+)".*/\1/')

    if [[ -z "$version" ]]; then
        error "Failed to fetch latest version from GitHub. Please check your internet connection or specify ENTIRE_VERSION."
    fi

    echo "$version"
}

download_file() {
    local url="$1"
    local output="$2"

    curl -fsSL "$url" -o "$output"
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
        error "Checksum verification failed!\nExpected: $expected_checksum\nActual: $actual_checksum"
    fi
}

main() {
    # Handle --help flag
    for arg in "$@"; do
        if [[ "$arg" == "--help" || "$arg" == "-h" ]]; then
            show_help
        fi
    done

    info "Installing Entire CLI..."

    # Detect platform
    local os arch
    os=$(detect_os)
    arch=$(detect_arch)
    info "Detected platform: ${os}/${arch}"

    # Determine version
    local version="${ENTIRE_VERSION:-}"
    if [[ -z "$version" ]]; then
        info "Fetching latest version..."
        version=$(get_latest_version)
    fi
    # Strip leading 'v' if present
    version="${version#v}"
    info "Installing version: ${version}"

    # Construct download URL
    local archive_name="${BINARY_NAME}_${os}_${arch}.tar.gz"
    local download_url="https://github.com/${GITHUB_REPO}/releases/download/v${version}/${archive_name}"
    local checksums_url="https://github.com/${GITHUB_REPO}/releases/download/v${version}/checksums.txt"

    # Create temp directory
    tmp_dir=$(mktemp -d)
    trap 'rm -rf "$tmp_dir"' EXIT

    # Download archive
    local archive_path="${tmp_dir}/${archive_name}"
    info "Downloading ${archive_name}..."
    if ! download_file "$download_url" "$archive_path"; then
        error "Failed to download from ${download_url}\nPlease check that the version exists and try again."
    fi

    # Verify checksum if available
    local checksums_path="${tmp_dir}/checksums.txt"
    if download_file "$checksums_url" "$checksums_path" 2>/dev/null; then
        info "Verifying checksum..."
        local expected_checksum
        expected_checksum=$(grep "${archive_name}" "$checksums_path" | awk '{print $1}')
        if [[ -n "$expected_checksum" ]]; then
            verify_checksum "$archive_path" "$expected_checksum"
            success "Checksum verified"
        else
            error "Checksum for ${archive_name} not found in checksums.txt"
        fi
    else
        warn "Checksums file not available. Skipping verification."
    fi

    # Extract archive
    info "Extracting..."
    tar -xzf "$archive_path" -C "$tmp_dir"

    # Determine install directory
    local install_dir="${ENTIRE_INSTALL_DIR:-$DEFAULT_INSTALL_DIR}"
    local binary_path="${tmp_dir}/${BINARY_NAME}"

    # Check if binary exists in extracted files
    if [[ ! -f "$binary_path" ]]; then
        # Some GoReleaser setups put binary in a subdirectory
        binary_path=$(find "$tmp_dir" -name "$BINARY_NAME" -type f | head -n 1)
        if [[ -z "$binary_path" || ! -f "$binary_path" ]]; then
            error "Could not find ${BINARY_NAME} binary in archive"
        fi
    fi

    # Make binary executable
    chmod +x "$binary_path"

    # Install binary
    info "Installing to ${install_dir}..."
    local install_path="${install_dir}/${BINARY_NAME}"

    # Install binary
    if [[ ! -d "$install_dir" ]]; then
        error "Install directory does not exist: ${install_dir}"
    fi
    if [[ ! -w "$install_dir" ]]; then
        error "Cannot write to ${install_dir}. Run with sudo or set ENTIRE_INSTALL_DIR to a writable location."
    fi
    mv "$binary_path" "$install_path"

    # Verify installation
    if "$install_path" --version &> /dev/null; then
        success "Entire CLI installed successfully!"
        echo ""
        echo "Run 'entire --help' to get started."
    else
        error "Installation completed but the binary failed to execute. Please check the installation."
    fi

    # Check if the installed binary is the one that will be found in PATH
    local path_binary
    path_binary=$(command -v "$BINARY_NAME" 2>/dev/null || true)
    if [[ -n "$path_binary" && "$path_binary" != "$install_path" ]]; then
        echo ""
        echo -e "${YELLOW}╔══════════════════════════════════════════════════════════════════╗${NC}"
        echo -e "${YELLOW}║${NC} ${BOLD}WARNING: PATH conflict detected!${NC}                                 ${YELLOW}║${NC}"
        echo -e "${YELLOW}╠══════════════════════════════════════════════════════════════════╣${NC}"
        echo -e "${YELLOW}║${NC} Installed to: ${install_path}$(printf '%*s' $((40 - ${#install_path})) '')${YELLOW}║${NC}"
        echo -e "${YELLOW}║${NC} But 'entire' resolves to: ${path_binary}$(printf '%*s' $((28 - ${#path_binary})) '')${YELLOW}║${NC}"
        echo -e "${YELLOW}╠══════════════════════════════════════════════════════════════════╣${NC}"
        echo -e "${YELLOW}║${NC} Your PATH may have another version earlier. To fix:              ${YELLOW}║${NC}"
        echo -e "${YELLOW}║${NC}   1. Remove the old binary: rm ${path_binary}$(printf '%*s' $((22 - ${#path_binary})) '')${YELLOW}║${NC}"
        echo -e "${YELLOW}║${NC}   2. Or adjust your PATH to prioritize ${install_dir}  ${YELLOW}║${NC}"
        echo -e "${YELLOW}╚══════════════════════════════════════════════════════════════════╝${NC}"
        echo ""
    elif [[ -z "$path_binary" ]]; then
        echo ""
        echo -e "${YELLOW}╔══════════════════════════════════════════════════════════════════╗${NC}"
        echo -e "${YELLOW}║${NC} ${BOLD}WARNING: 'entire' not found in PATH!${NC}                             ${YELLOW}║${NC}"
        echo -e "${YELLOW}╠══════════════════════════════════════════════════════════════════╣${NC}"
        echo -e "${YELLOW}║${NC} Installed to: ${install_path}$(printf '%*s' $((40 - ${#install_path})) '')${YELLOW}║${NC}"
        echo -e "${YELLOW}║${NC} But this directory is not in your PATH.                         ${YELLOW}║${NC}"
        echo -e "${YELLOW}╠══════════════════════════════════════════════════════════════════╣${NC}"
        echo -e "${YELLOW}║${NC} Add it to your shell config (.bashrc, .zshrc, etc.):           ${YELLOW}║${NC}"
        echo -e "${YELLOW}║${NC}   export PATH=\"\$PATH:${install_dir}\"$(printf '%*s' $((31 - ${#install_dir})) '')${YELLOW}║${NC}"
        echo -e "${YELLOW}╚══════════════════════════════════════════════════════════════════╝${NC}"
        echo ""
    fi
}

main "$@"
