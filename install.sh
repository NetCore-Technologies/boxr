#!/bin/bash
set -e
REPO="NetCore-Technologies/boxr"
VERSION=$(curl -s https://api.github.com/repos/$REPO/releases/latest | grep tag_name | cut -d '"' -f 4)
ARCH=$(uname -m)
[ "$ARCH" = "x86_64" ]  && ARCH="amd64"
[ "$ARCH" = "aarch64" ] && ARCH="arm64"
URL="https://github.com/$REPO/releases/download/$VERSION/boxr-$VERSION-linux-$ARCH.tar.gz"
echo "Downloading boxr $VERSION for linux/$ARCH..."
curl -sL $URL | tar -xz
sudo mv boxr-linux-$ARCH /usr/local/bin/boxr
echo "Done! Run: sudo boxr run /bin/sh"
