BINARY_NAME := kapsule
ROOTFS_DIR  := ./rootfs
GO_FILES    := $(wildcard *.go)

.PHONY: all build rootfs run test-pid test-hostname test-forkbomb clean help

all: build

help:
	@echo "Kapsule Commands:"
	@echo "  make build          Build static binary"
	@echo "  make rootfs         Download Alpine Linux rootfs"
	@echo "  make run            Run interactive shell (/bin/sh)"
	@echo "  make test-pid       Verify PID 1 inside container"
	@echo "  make test-hostname  Verify hostname isolation"
	@echo "  make test-forkbomb  Test Cgroups v2 fork bomb protection"
	@echo "  make clean          Remove build artifacts"

build: $(BINARY_NAME)

$(BINARY_NAME): $(GO_FILES)
	CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o $(BINARY_NAME) .

rootfs:
	@bash ./setup.sh

run: build
	@if [ ! -d "$(ROOTFS_DIR)" ]; then \
		echo "Run 'make rootfs' first to download the test rootfs."; \
		exit 1; \
	fi
	sudo ./$(BINARY_NAME) run $(ROOTFS_DIR) /bin/sh

test-pid: build
	sudo ./$(BINARY_NAME) run $(ROOTFS_DIR) /bin/ps aux

test-hostname: build
	@echo "Host hostname: $$(hostname)"
	@echo "Container hostname:"
	sudo ./$(BINARY_NAME) run $(ROOTFS_DIR) /bin/hostname

test-forkbomb: build
	@echo "Attempting 100 forks with pids.max=50 limit:"
	sudo ./$(BINARY_NAME) run $(ROOTFS_DIR) /bin/sh -c 'for i in $$(seq 1 100); do (sleep 10 &) || break; done; echo "Finished loop. Process list:"; ps aux'

clean:
	rm -f $(BINARY_NAME)
	rm -rf $(ROOTFS_DIR)
	rm -rf ./.cache
