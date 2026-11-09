#!/bin/sh
# ucfs installer
# Usage: curl -fsSL https://raw.githubusercontent.com/alxgsv/ucfs/main/scripts/install.sh | sh
#        curl -fsSL .../install.sh | sh -s -- --version 0.1.0 --install-dir ~/.local/bin
#
# Environment variables (flags of the same name take precedence):
#   VERSION       - version to install (e.g. "0.1.0"). Default: latest release.
#   INSTALL_DIR   - installation directory. Default: /usr/local/bin.
#   UNINSTALL     - set to "1" to uninstall. Example: curl ... | UNINSTALL=1 sh
#   UCFS_BASE_URL - directory holding checksums.txt and the archives, such as a
#                   local goreleaser dist/ served over HTTP. Default: the GitHub
#                   release of VERSION.

set -eu

REPO="alxgsv/ucfs"
BINARY="ucfs"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
VERSION="${VERSION:-}"
UNINSTALL="${UNINSTALL:-}"
BASE_URL="${UCFS_BASE_URL:-}"

IS_TTY=false
if [ -t 1 ]; then
    IS_TTY=true
fi

if [ "$IS_TTY" = true ]; then
    ESC=$(printf '\033')
    BAR_COLOR="${ESC}[38;2;255;205;0m"
    GREEN="${ESC}[0;32m"
    YELLOW="${ESC}[0;33m"
    RED="${ESC}[0;31m"
    WHITE="${ESC}[38;2;255;255;255m"
    GRAY="${ESC}[38;2;128;128;128m"
    SEPARATOR="${ESC}[38;2;68;68;68m"
    NC="${ESC}[0m"
    stage_done() { printf "${BAR_COLOR}✓${NC} ${WHITE}%s${NC}\n" "$1"; }
    stage_warn() { printf "${YELLOW}⚠${NC} ${WHITE}%s${NC}\n" "$1"; }
else
    BAR_COLOR="" GREEN="" YELLOW="" RED="" WHITE="" GRAY="" SEPARATOR="" NC=""
    stage_done() { echo "$1"; }
    stage_warn() { echo "Warning: $1"; }
fi

die() {
    printf "${RED}Error: %s${NC}\n" "$1" >&2
    exit 1
}

usage() {
    cat <<EOF
Install ucfs, which mounts an Uploadcare project as a local filesystem.

Usage: install.sh [--version X.Y.Z] [--install-dir DIR] [--base-url URL] [--uninstall]

  --version X.Y.Z    version to install (env VERSION); default: the latest release
  --install-dir DIR  where to put the binary (env INSTALL_DIR); default: /usr/local/bin
  --base-url URL     directory with checksums.txt and the archives (env UCFS_BASE_URL);
                     default: https://github.com/$REPO/releases/download/vX.Y.Z
  --uninstall        remove ucfs from the install dir (env UNINSTALL=1)
EOF
}

parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --version|--install-dir|--base-url)
                [ $# -ge 2 ] && [ -n "$2" ] && [ "${2#-}" = "$2" ] || die "$1 needs a value"
                case "$1" in
                    --version)     VERSION="$2" ;;
                    --install-dir) INSTALL_DIR="$2" ;;
                    --base-url)    BASE_URL="$2" ;;
                esac
                shift 2
                ;;
            --version=*)     VERSION="${1#*=}"; shift ;;
            --install-dir=*) INSTALL_DIR="${1#*=}"; shift ;;
            --base-url=*)    BASE_URL="${1#*=}"; shift ;;
            --uninstall)     UNINSTALL=1; shift ;;
            -h|--help)       usage; exit 0 ;;
            *)               usage >&2; die "unknown argument: $1" ;;
        esac
    done
    VERSION="${VERSION#v}"
    BASE_URL="${BASE_URL%/}"
}

# ── Uninstall ──────────────────────────────────────────────────────
uninstall() {
    _bin=""
    if [ -f "$INSTALL_DIR/$BINARY" ]; then
        _bin="$INSTALL_DIR/$BINARY"
    elif command -v "$BINARY" >/dev/null 2>&1; then
        _bin="$(command -v "$BINARY")"
    fi

    if [ -z "$_bin" ]; then
        printf "${YELLOW}ucfs is not installed.${NC}\n"
        exit 0
    fi

    _version=$("$_bin" version 2>/dev/null | head -1 || echo "unknown")
    printf "${GRAY}Found: %s (%s)${NC}\n" "$_bin" "$_version"

    if [ -w "$(dirname "$_bin")" ]; then
        rm -f "$_bin"
    else
        sudo rm -f "$_bin"
    fi

    printf "${GREEN}✓${NC} ${WHITE}Uninstalled ucfs from %s${NC}\n" "$_bin"
    exit 0
}

# ── Detect OS and architecture ─────────────────────────────────────
detect_platform() {
    OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
    case "$OS" in
        linux|darwin) ;;
        *) die "unsupported operating system: $OS (supported: darwin, linux)" ;;
    esac

    ARCH="$(uname -m)"
    case "$ARCH" in
        x86_64|amd64)  ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) die "unsupported architecture: $ARCH (supported: x86_64/amd64, aarch64/arm64)" ;;
    esac
}

# ── Resolve where the archive comes from ───────────────────────────
# Without a version, the archive is the one checksums.txt lists for this
# platform, so a base URL needs no version and GitHub's latest release needs
# no API call.
resolve_source() {
    if [ -z "$BASE_URL" ]; then
        if [ -n "$VERSION" ]; then
            BASE_URL="https://github.com/$REPO/releases/download/v$VERSION"
        else
            BASE_URL="https://github.com/$REPO/releases/latest/download"
        fi
    fi
}

# ── Download & Verify ───────────────────────────────────────────────
download_and_verify() {
    TMP="$(mktemp -d)"
    trap 'rm -rf "$TMP"' EXIT

    curl -fsSL "$BASE_URL/checksums.txt" -o "$TMP/checksums.txt" ||
        die "could not download $BASE_URL/checksums.txt; check the version (https://github.com/$REPO/releases)"

    if [ -n "$VERSION" ]; then
        ARCHIVE="${BINARY}_${VERSION}_${OS}_${ARCH}.tar.gz"
    else
        ARCHIVE="$(awk '{print $2}' "$TMP/checksums.txt" | grep "^${BINARY}_.*_${OS}_${ARCH}\.tar\.gz\$" || true)"
        [ "$(printf '%s\n' "$ARCHIVE" | grep -c .)" = 1 ] ||
            die "checksums.txt at $BASE_URL lists no single archive for $OS/$ARCH"
        VERSION="${ARCHIVE#"${BINARY}"_}"
        VERSION="${VERSION%_"${OS}"_"${ARCH}".tar.gz}"
    fi

    printf "${GRAY}Installing ${WHITE}ucfs ${GRAY}version: ${WHITE}%s${GRAY} (%s/%s)${NC}\n" "$VERSION" "$OS" "$ARCH"
    curl -fsSL "$BASE_URL/$ARCHIVE" -o "$TMP/$ARCHIVE" || die "failed to download $BASE_URL/$ARCHIVE"
    stage_done "Downloaded $ARCHIVE"

    EXPECTED="$(awk -v f="$ARCHIVE" '$2 == f {print $1}' "$TMP/checksums.txt")"
    [ -n "$EXPECTED" ] || die "$ARCHIVE not found in checksums.txt"

    if command -v sha256sum >/dev/null 2>&1; then
        ACTUAL="$(sha256sum "$TMP/$ARCHIVE" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
        ACTUAL="$(shasum -a 256 "$TMP/$ARCHIVE" | awk '{print $1}')"
    else
        ACTUAL=""
        stage_warn "Checksum skipped (no sha256sum or shasum)"
    fi
    if [ -n "$ACTUAL" ]; then
        [ "$EXPECTED" = "$ACTUAL" ] || die "checksum mismatch for $ARCHIVE: expected $EXPECTED, got $ACTUAL"
        stage_done "Checksum verified"
    fi

    tar -xzf "$TMP/$ARCHIVE" -C "$TMP" "$BINARY" || die "$ARCHIVE holds no $BINARY binary"
}

# ── Install Binary ──────────────────────────────────────────────────
install_binary() {
    if [ ! -d "$INSTALL_DIR" ] && ! mkdir -p "$INSTALL_DIR" 2>/dev/null; then
        printf "${GRAY}Creating %s requires elevated privileges.${NC}\n" "$INSTALL_DIR"
        sudo mkdir -p "$INSTALL_DIR"
    fi
    if [ -w "$INSTALL_DIR" ]; then
        cp "$TMP/$BINARY" "$INSTALL_DIR/$BINARY"
        chmod 755 "$INSTALL_DIR/$BINARY"
    else
        printf "${GRAY}Installing to %s requires elevated privileges.${NC}\n" "$INSTALL_DIR"
        sudo cp "$TMP/$BINARY" "$INSTALL_DIR/$BINARY"
        sudo chmod 755 "$INSTALL_DIR/$BINARY"
    fi
    _installed="$("$INSTALL_DIR/$BINARY" version)" || die "$INSTALL_DIR/$BINARY does not run"
    stage_done "Installed $_installed to $INSTALL_DIR"
}

# ── Success Message ─────────────────────────────────────────────────
print_success() {
    case ":$PATH:" in
        *":$INSTALL_DIR:"*) ;;
        *) stage_warn "$INSTALL_DIR is not in PATH" ;;
    esac
    if [ "$OS" = linux ] && ! command -v fusermount3 >/dev/null 2>&1; then
        stage_warn "fusermount3 not found: install the fuse3 package to mount"
    fi
    printf "\n"
    printf "${GRAY}Mount a project (keys from ~/.uploadcare/config.yaml or UPLOADCARE_PUBLIC_KEY/UPLOADCARE_SECRET_KEY):${NC}\n"
    printf "${WHITE}mkdir -p ~/Uploadcare && ucfs mount --daemon ~/Uploadcare${NC}\n"
    printf "${WHITE}ls ~/Uploadcare/files${NC}\n"
    printf "${WHITE}ucfs umount ~/Uploadcare${NC}\n"
    printf "\n"
    printf "%s\n" "${SEPARATOR}-----------------------------------------------------------${NC}"
    printf "${GRAY}Docs: https://github.com/%s#readme${NC}\n" "$REPO"
}

main() {
    parse_args "$@"
    if [ "$UNINSTALL" = "1" ]; then
        uninstall
    fi
    if command -v "$BINARY" >/dev/null 2>&1; then
        printf "${SEPARATOR}Currently installed: %s${NC}\n" "$("$BINARY" version 2>/dev/null | head -1 || echo unknown)"
    fi
    command -v curl >/dev/null 2>&1 || die "curl is required"
    detect_platform
    resolve_source
    download_and_verify
    install_binary
    print_success
}

main "$@"
