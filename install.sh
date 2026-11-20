#!/usr/bin/env bash
set -euo pipefail

REPO="NetCore-Technologies/boxr"
API="https://api.github.com/repos/${REPO}/releases/latest"

if [ -n "${BOXR_VERSION:-}" ]; then
  VERSION="$BOXR_VERSION"
else
  VERSION="$(curl --fail --silent --show-error --location --retry 3 "$API" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
fi
if [ -z "$VERSION" ]; then
  echo "error: could not determine the latest boxr release" >&2
  exit 1
fi

case "$(uname -s)" in
  Linux) ;;
  *) echo "error: container execution requires Linux" >&2; exit 1 ;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "error: unsupported Linux architecture: $ARCH" >&2; exit 1 ;;
esac

ASSET="boxr-${VERSION}-linux-${ARCH}.tar.gz"
CHECKSUMS="boxr-${VERSION}-checksums.txt"
BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

curl --fail --silent --show-error --location --retry 3 "$BASE_URL/$ASSET" -o "$TMP_DIR/$ASSET"
curl --fail --silent --show-error --location --retry 3 "$BASE_URL/$CHECKSUMS" -o "$TMP_DIR/$CHECKSUMS"

if ! grep -F "  ${ASSET}" "$TMP_DIR/$CHECKSUMS" > "$TMP_DIR/expected-checksum"; then
  echo "error: release checksum manifest does not contain ${ASSET}" >&2
  exit 1
fi
(cd "$TMP_DIR" && sha256sum --check expected-checksum)

tar -xzf "$TMP_DIR/$ASSET" -C "$TMP_DIR"
INSTALL_DIR="${BOXR_INSTALL_DIR:-/usr/local/bin}"
mkdir -p "$TMP_DIR/install"
install -m 0755 "$TMP_DIR/boxr-linux-${ARCH}" "$TMP_DIR/install/boxr"

if install -d "$INSTALL_DIR" 2>/dev/null; then
  install -m 0755 "$TMP_DIR/install/boxr" "$INSTALL_DIR/boxr"
elif command -v sudo >/dev/null 2>&1; then
  sudo install -d "$INSTALL_DIR"
  sudo install -m 0755 "$TMP_DIR/install/boxr" "$INSTALL_DIR/boxr"
else
  echo "error: cannot write to ${INSTALL_DIR}; set BOXR_INSTALL_DIR to a writable directory" >&2
  exit 1
fi

echo "Installed boxr ${VERSION} to ${INSTALL_DIR}/boxr"
echo "Run 'boxr version' to verify the installation."
