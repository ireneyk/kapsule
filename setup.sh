#!/usr/bin/env bash
# Downloads and unpacks minimal Alpine Linux rootfs for testing Kapsule.
set -euo pipefail

ROOTFS_DIR="./rootfs"
ALPINE_VERSION="3.20.0"
CACHE_DIR="./.cache"

for tool in curl tar; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "Error: $tool is required." >&2
        exit 1
    fi
done

# Detect CPU architecture
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64) ALPINE_ARCH="x86_64" ;;
    aarch64|arm64) ALPINE_ARCH="aarch64" ;;
    armv7l|armhf)  ALPINE_ARCH="armhf" ;;
    *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

TARBALL_NAME="alpine-minirootfs-${ALPINE_VERSION}-${ALPINE_ARCH}.tar.gz"
DOWNLOAD_URL="https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/${ALPINE_ARCH}/${TARBALL_NAME}"

mkdir -p "$CACHE_DIR"

if [ ! -f "${CACHE_DIR}/${TARBALL_NAME}" ]; then
    echo "Downloading Alpine Linux rootfs (${ALPINE_ARCH})..."
    curl -fSL "${DOWNLOAD_URL}" -o "${CACHE_DIR}/${TARBALL_NAME}"
fi

if [ -d "$ROOTFS_DIR" ]; then
    echo "Removing previous rootfs..."
    rm -rf "$ROOTFS_DIR"
fi

echo "Unpacking into ${ROOTFS_DIR}..."
mkdir -p "$ROOTFS_DIR"
tar -xzf "${CACHE_DIR}/${TARBALL_NAME}" -C "$ROOTFS_DIR"

# Set up basic DNS
mkdir -p "${ROOTFS_DIR}/etc"
if [ -f /etc/resolv.conf ]; then
    cp /etc/resolv.conf "${ROOTFS_DIR}/etc/resolv.conf" 2>/dev/null || true
else
    echo "nameserver 1.1.1.1" > "${ROOTFS_DIR}/etc/resolv.conf"
fi

echo "Rootfs ready! You can now run:"
echo "  sudo ./kapsule run ./rootfs /bin/sh"
