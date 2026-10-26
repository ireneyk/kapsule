# ==============================================================================
# Makefile: Kapsule - A Raw Linux Container Runtime
# Author:   Irene
#
# Commands:
#   make build          - Compiles the static Go binary
#   make rootfs         - Downloads & unpacks minimal Alpine Linux rootfs
#   make run            - Starts an interactive container shell (/bin/sh)
#   make test-pid       - Verifies PID namespace isolation (ps aux)
#   make test-hostname  - Verifies UTS namespace hostname isolation
#   make test-forkbomb  - Demonstrates Cgroup v2 fork bomb mitigation
#   make clean          - Removes binaries and rootfs
# ==============================================================================

BINARY_NAME := kapsule
ROOTFS_DIR  := ./rootfs
GO_FILES    := $(wildcard *.go)

.PHONY: all build rootfs run test-pid test-hostname test-forkbomb clean help

all: build

help:
	@echo "Kapsule Build System (Author: Irene)"
	@echo ""
	@echo "Targets:"
	@echo "  make build          Compile the static Kapsule binary"
	@echo "  make rootfs         Download and extract Alpine Linux rootfs"
	@echo "  make run            Run interactive container shell"
	@echo "  make test-pid       Verify PID 1 virtualization"
	@echo "  make test-hostname  Verify UTS hostname isolation"
	@echo "  make test-forkbomb  Verify Cgroups v2 fork bomb protection"
	@echo "  make clean          Clean build artifacts and rootfs"

build: $(BINARY_NAME)

$(BINARY_NAME): $(GO_FILES)
	@echo "==> Compiling static Go binary [$(BINARY_NAME)]..."
	CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o $(BINARY_NAME) .
	@echo "==> Build complete: $(BINARY_NAME)"

rootfs:
	@echo "==> Preparing Alpine root filesystem..."
	@bash ./setup.sh

run: build
	@if [ ! -d "$(ROOTFS_DIR)" ]; then \
		echo "Error: $(ROOTFS_DIR) not found. Run 'make rootfs' first."; \
		exit 1; \
	fi
	@echo "==> Launching interactive container session..."
	sudo ./$(BINARY_NAME) run $(ROOTFS_DIR) /bin/sh

test-pid: build
	@echo "==> Testing PID Namespace Isolation (Expected: child is PID 1)..."
	sudo ./$(BINARY_NAME) run $(ROOTFS_DIR) /bin/ps aux

test-hostname: build
	@echo "==> Testing UTS Namespace Isolation..."
	@echo "Host Hostname:"
	@hostname
	@echo "Container Hostname:"
	sudo ./$(BINARY_NAME) run $(ROOTFS_DIR) /bin/hostname

test-forkbomb: build
	@echo "==> Testing Cgroups v2 Fork Bomb Protection..."
	@echo "Attempting to spawn 100 background processes inside container..."
	@echo "Cgroup limit is pids.max = 50. The kernel will reject excessive forks with EAGAIN."
	sudo ./$(BINARY_NAME) run $(ROOTFS_DIR) /bin/sh -c 'for i in $$(seq 1 100); do (sleep 10 &) || break; done; echo "Spawn loop finished. Active container processes:"; ps aux'

clean:
	@echo "==> Cleaning up build artifacts..."
	rm -f $(BINARY_NAME)
	@echo "==> Removing rootfs..."
	rm -rf $(ROOTFS_DIR)
	@echo "==> Removing cache..."
	rm -rf ./.cache
	@echo "==> Done."
