# Kapsule

A minimal, dependency-free Linux container runtime written from scratch in pure Go.

---

## Why I Built This

I wanted to understand what containers *actually* are under the hood without relying on Docker, Podman, or `runc`. 

Containers don't exist as dedicated objects in the Linux kernel. A container is just a normal Linux process with:
1. **Namespaces** to isolate what it can **see** (PIDs, hostname, mounts).
2. **Cgroups** to limit what it can **use** (memory, CPU, process count).
3. **`pivot_root`** to trap it inside its own root filesystem so it can't escape into the host.

Kapsule implements all three using raw Linux system calls in standard library Go.

---

## Architecture Flow

```
[ Host Shell ]
      │
      ▼  sudo ./kapsule run ./rootfs /bin/sh
┌─────────────────────────────────────────────────────────────┐
│ 1. Parent Process (Supervisor)                              │
│    - Checks root privileges                                 │
│    - Creates a synchronization pipe                         │
│    - Re-executes /proc/self/exe with clone flags:           │
│        • CLONE_NEWUTS (hostname isolation)                  │
│        • CLONE_NEWPID (child becomes PID 1)                 │
│        • CLONE_NEWNS  (mount table isolation)               │
└──────────────────────────────┬──────────────────────────────┘
                               │
            ┌──────────────────┴──────────────────┐
            ▼                                     ▼
┌───────────────────────────────┐   ┌───────────────────────────────┐
│ Parent                        │   │ Child (in new namespaces)     │
│ - Puts child into Cgroup v2:  │   │ - Waits on sync pipe          │
│     • pids.max = 50           │   │                               │
│     • memory.max = 100MB      │   │                               │
│     • cpu.max = 50% quota     │   │                               │
│ - Sends 1 byte to sync pipe ──┼──>│ - Unblocks!                   │
│ - Waits for container exit    │   │ - Sets hostname to "kapsule"  │
│ - Cleans up cgroup slice      │   │ - pivot_root to new rootfs    │
└───────────────────────────────┘   │ - Lazy unmounts old host root │
                                    │ - Mounts fresh /proc          │
                                    │ - Replaces itself via execve  │
                                    └───────────────┬───────────────┘
                                                    │
                                                    ▼
                                    ┌───────────────────────────────┐
                                    │ Target Process (/bin/sh)      │
                                    │ (Runs as PID 1 inside rootfs) │
                                    └───────────────────────────────┘
```

---

## Core Concepts Explained Simply

### 1. Namespaces vs Cgroups
- **Namespaces = Isolation (What you can SEE):**  
  - `CLONE_NEWUTS`: Container gets its own hostname without touching the host.
  - `CLONE_NEWPID`: Process ID virtualization. The child process thinks it is PID 1.
  - `CLONE_NEWNS`: Mount table isolation. Mounts made inside the container (like `/proc`) stay private and won't leak to the host.
- **Cgroups = Limits (What you can USE):**  
  - Namespaces don't stop a process from eating 100% of your RAM or crashing the host. Cgroups enforce hard limits on hardware resources.

### 2. `pivot_root` vs `chroot`
- Many basic tutorials use `chroot`, but **`chroot` is not secure**. It only changes the root path pointer for the process. A root process can break out using `fchdir()` on an open file descriptor outside the directory.
- **`pivot_root`** swaps the actual mount points at the kernel level. After pivoting, we unmount the old host root using `syscall.Unmount("/.oldroot", syscall.MNT_DETACH)`. The host filesystem is completely removed from the container's mount table, making escapes impossible.

### 3. The "Re-exec" Pattern
- In Go, the runtime starts multiple background OS threads. Linux namespaces are assigned per-thread. If you try to unshare namespaces in a running Go program, goroutines can jump across threads and fall back into the host namespace.
- Calling `/proc/self/exe child` forks a clean, single-threaded copy of our program directly inside the newly created namespaces.

### 4. Preventing Fork Bombs (Cgroups v2)
- Without limits, running a fork bomb (`:(){ :|:& };:`) creates thousands of processes until the kernel PID table is full, locking up the whole computer.
- Kapsule sets `pids.max = 50` in Cgroups v2. Once the container reaches 50 processes, any new `fork()` fails with `Resource temporarily unavailable`. The host stays 100% responsive.

---

## Real-World Lessons: Debugging Kubernetes & OpenShift

Building this runtime directly explains common cloud issues:

1. **`CrashLoopBackOff` & OOMKilled (Exit Code 137):**  
   Kubernetes memory limits (`resources.limits.memory`) translate directly to Cgroup v2 `memory.max`. When a container crosses that limit, the kernel OOM killer sends `SIGKILL` (128 + 9 = 137). Inspecting `/sys/fs/cgroup/memory.events` shows the exact `oom_kill` count.
2. **PID Starvation & Worker Node Freezes:**  
   If containers don't have a `pids.max` limit, a thread-leaking app can exhaust the host kernel PID table. Kubernetes uses `SupportNodePidsLimit` and OpenShift uses `KubeletConfig` to enforce `pids.max` on pods.
3. **Zombie Processes in Containers:**  
   Inside a container, PID 1 has the job of adopting and reaping orphaned processes (`SIGCHLD`). If your container entrypoint is a simple script that doesn't handle child signals, dead processes remain as `[defunct]` zombies until the PID limit is reached.

---

## Quickstart

### Prerequisites
- Linux machine, VM, or WSL2 with systemd and Cgroups v2
- Go 1.20+
- `make`, `curl`, `tar`, `sudo`

### 1. Download Alpine rootfs
```bash
make rootfs
```
*(Runs `setup.sh` to grab the minimal Alpine Linux root filesystem into `./rootfs`)*

### 2. Build the binary
```bash
make build
```

### 3. Launch an isolated container
```bash
make run
```

Inside the shell:
```sh
# Verify PID 1
ps aux

# Verify isolated hostname
hostname

# Verify isolated filesystem
ls /
```

### 4. Test Fork Bomb Protection
```bash
make test-forkbomb
```
You will see `can't fork: Resource temporarily unavailable` when the process count hits 50, proving that Cgroups stopped the fork bomb without affecting the host.

---

## Project Structure

```
kapsule/
├── main.go        # Complete container runtime in pure Go
├── setup.sh       # Downloads minimal Alpine rootfs
├── Makefile       # Simple build and test targets
├── go.mod         # Go module definition
├── LICENSE        # MIT License
└── README.md      # Project documentation
```
