#!/usr/bin/env bash
# ==============================================================================
# Script:  setup.sh
# Project: Kapsule - A Raw Linux Container Runtime
# Author:  Irene
#
# Description:
# Automated environment preparation script. Detects CPU architecture, downloads
# the minimal Alpine Linux root filesystem (rootfs), and extracts it into the
# local ./rootfs directory for container runtime execution and testing.
# ==============================================================================

set -euo pipefail

ROOTFS_DIR="./rootfs"
ALPINE_VERSION="3.20.0"
CACHE_DIR="./.cache"

# ANSI Colors for terminal output
BOLD="\033[1m"
GREEN="\033[0;32m"
BLUE="\033[0;34m"
YELLOW="\033[1;33m"
RED="\033[0;31m"
RESET="\033[0m"

echo -e "${BOLD}${BLUE}======================================================${RESET}"
echo -e "${BOLD}${BLUE}   Kapsule: Container Runtime Environment Setup       ${RESET}"
echo -e "${BOLD}${BLUE}   Author: Irene                                      ${RESET}"
echo -e "${BOLD}${BLUE}======================================================${RESET}"

# 1. Check required tools
for tool in curl tar; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo -e "${RED}[ERROR] Required utility '$tool' is not installed.${RESET}" >&2
        exit 1
    fi
done

# 2. Detect CPU Architecture
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64)
        ALPINE_ARCH="x86_64"
        ;;
    aarch64|arm64)
        ALPINE_ARCH="aarch64"
        ;;
    armv7l|armhf)
        ALPINE_ARCH="armhf"
        ;;
    *)
        echo -e "${RED}[ERROR] Unsupported architecture: $ARCH${RESET}" >&2
        exit 1
        ;;
esac

TARBALL_NAME="alpine-minirootfs-${ALPINE_VERSION}-${ALPINE_ARCH}.tar.gz"
DOWNLOAD_URL="https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/${ALPINE_ARCH}/${TARBALL_NAME}"

mkdir -p "$CACHE_DIR"

# 3. Download Alpine Mini-Rootfs if not already cached
if [ ! -f "${CACHE_DIR}/${TARBALL_NAME}" ]; then
    echo -e "${YELLOW}[DOWNLOAD] Fetching Alpine Linux minirootfs (${ALPINE_ARCH})...${RESET}"
    echo -e "           URL: ${DOWNLOAD_URL}"
    curl -fSL --progress-bar "${DOWNLOAD_URL}" -o "${CACHE_DIR}/${TARBALL_NAME}"
    echo -e "${GREEN}[OK] Download complete.${RESET}"
else
    echo -e "${GREEN}[CACHE] Using cached tarball: ${CACHE_DIR}/${TARBALL_NAME}${RESET}"
fi

# 4. Extract into rootfs directory
if [ -d "$ROOTFS_DIR" ] && [ "$(ls -A "$ROOTFS_DIR" 2>/dev/null)" ]; then
    echo -e "${YELLOW}[NOTICE] Target directory '$ROOTFS_DIR' already exists and is non-empty.${RESET}"
    read -p "Do you want to re-extract and overwrite? (y/N): " -r CONFIRM
    if [[ ! "$CONFIRM" =~ ^[Yy]$ ]]; then
        echo -e "${BLUE}[INFO] Keeping existing rootfs.${RESET}"
        exit 0
    fi
    echo -e "${YELLOW}[CLEAN] Removing previous rootfs...${RESET}"
    rm -rf "$ROOTFS_DIR"
fi

echo -e "${YELLOW}[EXTRACT] Unpacking root filesystem into ${ROOTFS_DIR}...${RESET}"
mkdir -p "$ROOTFS_DIR"
tar -xzf "${CACHE_DIR}/${TARBALL_NAME}" -C "$ROOTFS_DIR"

# 5. Populate basic network configuration
# Ensuring DNS works out of the box inside container
if [ -f /etc/resolv.conf ]; then
    echo -e "${YELLOW}[CONFIG] Copying host DNS config (/etc/resolv.conf)...${RESET}"
    mkdir -p "${ROOTFS_DIR}/etc"
    cp /etc/resolv.conf "${ROOTFS_DIR}/etc/resolv.conf" 2>/dev/null || true
else
    echo -e "${YELLOW}[CONFIG] Writing default DNS nameserver (1.1.1.1)...${RESET}"
    mkdir -p "${ROOTFS_DIR}/etc"
    echo "nameserver 1.1.1.1" > "${ROOTFS_DIR}/etc/resolv.conf"
fi

echo -e "${BOLD}${GREEN}======================================================${RESET}"
echo -e "${BOLD}${GREEN}[SUCCESS] Root filesystem is ready!${RESET}"
echo -e "${BOLD}${GREEN}======================================================${RESET}"
echo -e "Next steps:"
echo -e "  1. Compile runtime:   ${BOLD}make build${RESET}"
echo -e "  2. Launch container:  ${BOLD}sudo ./kapsule run ./rootfs /bin/sh${RESET}"
echo -e "  3. Test PID 1:        ${BOLD}sudo ./kapsule run ./rootfs /bin/ps aux${RESET}"
