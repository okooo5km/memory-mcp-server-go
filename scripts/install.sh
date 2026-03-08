#!/usr/bin/env bash
set -euo pipefail

APP_NAME="mms"
REPO="okooo5km/memory-mcp-server-go"

VERSION="latest"
DEST_DIR="${HOME}/.local/bin"
QUIET=false

usage() {
  cat <<USAGE
${APP_NAME} installer

Options:
  -v, --version   Version tag to install (e.g. v0.2.3). Default: latest
  -d, --dest      Install destination directory. Default: ${HOME}/.local/bin
  -q, --quiet     Less verbose output
  -h, --help      Show this help

Examples:
  curl -fsSL https://raw.githubusercontent.com/${REPO}/main/scripts/install.sh | bash
  curl -fsSL https://raw.githubusercontent.com/${REPO}/main/scripts/install.sh | bash -s -- -v v0.2.3 -d /usr/local/bin
USAGE
}

log() {
  if [ "$QUIET" = false ]; then
    echo "$@"
  fi
}

err() { echo "[ERROR] $*" >&2; }

# Parse args
while [ $# -gt 0 ]; do
  case "$1" in
    -v|--version) VERSION="$2"; shift 2;;
    -d|--dest) DEST_DIR="$2"; shift 2;;
    -q|--quiet) QUIET=true; shift;;
    -h|--help) usage; exit 0;;
    *) err "Unknown argument: $1"; usage; exit 1;;
  esac
done

# Detect platform
uname_s=$(uname -s | tr '[:upper:]' '[:lower:]')
uname_m=$(uname -m | tr '[:upper:]' '[:lower:]')

case "$uname_s" in
  linux)   os="linux" ; ext=".tar.gz" ; ;;
  darwin)  os="darwin"; ext=".tar.gz" ; ;;
  msys*|mingw*|cygwin*)
    err "Windows shell installation is not supported. Please download the .zip from Releases and extract manually."
    exit 1
    ;;
  *) err "Unsupported OS: $uname_s"; exit 1;;
esac

case "$uname_m" in
  x86_64|amd64) arch="amd64" ; ;;
  aarch64|arm64) arch="arm64" ; ;;
  *) err "Unsupported architecture: $uname_m"; exit 1;;
esac

# Resolve version
if [ "$VERSION" = "latest" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')
  if [ -z "$VERSION" ]; then
    err "Failed to fetch latest version from GitHub API"
    exit 1
  fi
  log "Latest version: ${VERSION}"
fi

# Ensure version has leading v
case "$VERSION" in v*) ;; *) VERSION="v${VERSION}";; esac

# GoReleaser archive naming: mms_VERSION_os_arch.tar.gz
version_no_v="${VERSION#v}"
asset_name="${APP_NAME}_${version_no_v}_${os}_${arch}${ext}"
url="https://github.com/${REPO}/releases/download/${VERSION}/${asset_name}"

tmpdir=$(mktemp -d)
cleanup() { rm -rf "$tmpdir"; }
trap cleanup EXIT INT TERM

archive_path="${tmpdir}/${asset_name}"

log "Downloading ${asset_name} from ${url} ..."
curl -fL ${QUIET:+-sS} -o "$archive_path" "$url"

log "Extracting..."
tar -xzf "$archive_path" -C "$tmpdir"

# GoReleaser extracts binary as APP_NAME directly
src_bin="${tmpdir}/${APP_NAME}"
if [ ! -f "$src_bin" ]; then
  err "Extracted binary not found: $src_bin"
  exit 1
fi

chmod +x "$src_bin"

# macOS: attempt to clear quarantine silently if present
if [ "$os" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
  xattr -p com.apple.quarantine "$src_bin" >/dev/null 2>&1 && xattr -d com.apple.quarantine "$src_bin" || true
fi

mkdir -p "$DEST_DIR"

log "Installing to ${DEST_DIR}/${APP_NAME}"
mv "$src_bin" "${DEST_DIR}/${APP_NAME}"

if ! command -v "$DEST_DIR/${APP_NAME}" >/dev/null 2>&1; then
  log "Installed. Ensure ${DEST_DIR} is in your PATH."
else
  log "Installed: $("${DEST_DIR}/${APP_NAME}" --version 2>/dev/null || echo ${DEST_DIR}/${APP_NAME})"
fi

log "Done."
