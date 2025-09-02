#!/usr/bin/env bash
set -euo pipefail

APP_NAME="memory-mcp-server-go"
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
  linux)   os="linux" ; ext=".tgz" ; ;;
  darwin)  os="darwin"; ext=".tgz" ; ;;
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

asset_name="${APP_NAME}-${os}-${arch}${ext}"

if [ "$VERSION" = "latest" ]; then
  url="https://github.com/${REPO}/releases/latest/download/${asset_name}"
else
  # ensure version has leading v
  case "$VERSION" in v*) ;; *) VERSION="v${VERSION}";; esac
  url="https://github.com/${REPO}/releases/download/${VERSION}/${asset_name}"
fi

tmpdir=$(mktemp -d)
cleanup() { rm -rf "$tmpdir"; }
trap cleanup EXIT INT TERM

archive_path="${tmpdir}/${asset_name}"

log "Downloading ${asset_name} from ${url} ..."
curl -fL ${QUIET:+-sS} -o "$archive_path" "$url"

log "Extracting..."
tar -xzf "$archive_path" -C "$tmpdir"

# Determine extracted binary name (should be APP_NAME-os-arch)
bin_name="${APP_NAME}-${os}-${arch}"
if [ ! -f "${tmpdir}/${bin_name}" ]; then
  # Fallback to first entry in archive
  bin_name=$(tar -tzf "$archive_path" | head -1)
fi

src_bin="${tmpdir}/${bin_name}"
if [ ! -f "$src_bin" ]; then
  err "Extracted binary not found: $src_bin"
  exit 1
fi

chmod +x "$src_bin"

# macOS: attempt to clear quarantine silently if present
if [ "$os" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
  xattr -p com.apple.quarantine "$src_bin" >/dev/null 2>&1 && xattr -d com.apple.quarantine "$src_bin" || true
fi

install_name="${APP_NAME}"
mkdir -p "$DEST_DIR"

log "Installing to ${DEST_DIR}/${install_name}"
mv "$src_bin" "${DEST_DIR}/${install_name}"

if ! command -v "$DEST_DIR/${install_name}" >/dev/null 2>&1; then
  log "Installed. Ensure ${DEST_DIR} is in your PATH."
else
  log "Installed: $("${DEST_DIR}/${install_name}" --version 2>/dev/null || echo ${DEST_DIR}/${install_name})"
fi

log "Done."
