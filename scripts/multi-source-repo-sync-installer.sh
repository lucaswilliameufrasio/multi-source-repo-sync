#!/usr/bin/env sh
set -eu

# multi-source-repo-sync installer. Release assets are verified with their published SHA-256
# checksum. This protects against transport corruption, not a compromised release.

REPO="lucaswilliameufrasio/multi-source-repo-sync"
TAG="latest"
BIN_DIR="${HOME}/.local/bin"
FORCE=""

usage() {
  cat <<USAGE
multi-source-repo-sync installer

Usage:
  multi-source-repo-sync-installer.sh [--repo <owner/repo>] [--tag <vX.Y.Z|latest>] [--bin-dir <path>]

Options:
  --repo      GitHub repository in owner/repo format
  --tag       Release tag (default: latest)
  --bin-dir   Install directory (default: ~/.local/bin)
  --force     Replace an existing installation without prompting
  -h, --help  Show this help
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --repo)
      [ "$#" -ge 2 ] || { echo "--repo requires a value" >&2; exit 1; }
      REPO="$2"
      shift 2
      ;;
    --tag)
      [ "$#" -ge 2 ] || { echo "--tag requires a value" >&2; exit 1; }
      TAG="$2"
      shift 2
      ;;
    --bin-dir)
      [ "$#" -ge 2 ] || { echo "--bin-dir requires a value" >&2; exit 1; }
      BIN_DIR="$2"
      shift 2
      ;;
    --force)
      FORCE="yes"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 1; }
command -v tar >/dev/null 2>&1 || { echo "tar is required" >&2; exit 1; }

OS="$(uname -s)"
ARCH="$(uname -m)"
case "$OS" in
  Linux) OS_NAME="linux" ;;
  Darwin) OS_NAME="darwin" ;;
  *)
    echo "Unsupported OS: $OS. Download a release artifact manually." >&2
    exit 1
    ;;
esac

case "$ARCH" in
  x86_64|amd64) ARCH_NAME="amd64" ;;
  arm64|aarch64) ARCH_NAME="arm64" ;;
  *)
    echo "Unsupported architecture: $ARCH" >&2
    exit 1
    ;;
esac

if [ "$TAG" = "latest" ]; then
  RELEASE_JSON="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest")"
  TAG="$(printf '%s' "$RELEASE_JSON" | tr -d '\n' | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
  [ -n "$TAG" ] || { echo "Failed to resolve latest release tag for $REPO" >&2; exit 1; }
fi

VERSION="${TAG#v}"
ASSET="multi-source-repo-sync_${VERSION}_${OS_NAME}_${ARCH_NAME}.tar.gz"
BASE_URL="https://github.com/${REPO}/releases/download/${TAG}"
TMP_DIR="$(mktemp -d 2>/dev/null || mktemp -d -t multi-source-repo-sync-install)"
cleanup() { rm -rf "$TMP_DIR"; }
trap cleanup EXIT INT TERM

ASSET_FILE="${TMP_DIR}/${ASSET}"
CHECKSUM_FILE="${ASSET_FILE}.sha256"
echo "Downloading ${BASE_URL}/${ASSET}"
curl -fsSL "${BASE_URL}/${ASSET}" -o "$ASSET_FILE"
curl -fsSL "${BASE_URL}/${ASSET}.sha256" -o "$CHECKSUM_FILE"

EXPECTED_HASH="$(awk '{print $1}' "$CHECKSUM_FILE")"
[ -n "$EXPECTED_HASH" ] || { echo "Invalid checksum file" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL_HASH="$(sha256sum "$ASSET_FILE" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL_HASH="$(shasum -a 256 "$ASSET_FILE" | awk '{print $1}')"
else
  echo "sha256sum or shasum is required for checksum verification" >&2
  exit 1
fi
[ "$EXPECTED_HASH" = "$ACTUAL_HASH" ] || { echo "Checksum mismatch for ${ASSET}" >&2; exit 1; }
echo "Checksum verified"

tar -xf "$ASSET_FILE" -C "$TMP_DIR"
[ -f "${TMP_DIR}/multi-source-repo-sync" ] || { echo "Archive does not contain multi-source-repo-sync" >&2; exit 1; }
mkdir -p "$BIN_DIR"
if [ -e "${BIN_DIR}/multi-source-repo-sync" ] && [ "$FORCE" != "yes" ]; then
  printf 'Replace %s? [y/N] ' "${BIN_DIR}/multi-source-repo-sync"
  if [ -r /dev/tty ]; then
    read -r answer </dev/tty || answer=n
  else
    answer=n
    echo "non-interactive, keeping existing installation" >&2
    echo "Use --force to replace it" >&2
  fi
  case "$answer" in [Yy]*) ;; *) echo "Installation cancelled"; exit 1 ;; esac
fi
install -m 0755 "${TMP_DIR}/multi-source-repo-sync" "${BIN_DIR}/multi-source-repo-sync"

echo "Installed multi-source-repo-sync ${TAG} to ${BIN_DIR}/multi-source-repo-sync"
case ":${PATH}:" in
  *:"${BIN_DIR}":*) ;;
  *) echo "Add ${BIN_DIR} to PATH: export PATH=\"${BIN_DIR}:\$PATH\"" ;;
esac
