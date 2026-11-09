#!/bin/sh
# install.sh: download bootstrap only. It detects OS/arch, downloads the
# matching km binary, verifies the published checksum, then execs the built-in
# installer (km install). Install, init and systemd logic live in km itself so
# a manual download can also run `sudo ./km install`.
set -eu

: "${KM_VERSION:?set KM_VERSION, e.g. KM_VERSION=v1.2.3 ./install.sh}"
: "${KM_BASE_URL:?set KM_BASE_URL, e.g. https://releases.example.com/km}"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "install.sh: unsupported arch $ARCH" >&2; exit 1 ;;
esac
case "$OS" in
  linux|darwin) ;;
  *) echo "install.sh: unsupported os $OS (systemd install supports Linux; other platforms keep single-file mode)" >&2; exit 1 ;;
esac

NAME="km-${KM_VERSION}-${OS}-${ARCH}"
URL="${KM_BASE_URL}/${NAME}"
SUM_URL="${URL}.sha256"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

echo "下载 $URL"
if command -v curl >/dev/null 2>&1; then
  curl -fsSL -o "$TMP/km" "$URL"
  curl -fsSL -o "$TMP/km.sha256" "$SUM_URL"
elif command -v wget >/dev/null 2>&1; then
  wget -O "$TMP/km" "$URL"
  wget -O "$TMP/km.sha256" "$SUM_URL"
else
  echo "install.sh: 需要 curl 或 wget" >&2; exit 1
fi

# Verify against the published checksum mechanism (no invented signatures).
(cd "$TMP" && sha256sum -c km.sha256)
chmod +x "$TMP/km"

# Hand over to the built-in installer (reads /dev/tty when piped).
exec "$TMP/km" install "$@"
