# Kapsule: A Raw Linux Container Runtime

**Author:** Irene  
**Language:** Pure Go (Standard Library & `syscall` only)  
**Platform:** Linux Kernel 5.8+ (x86_64, aarch64)  
**License:** Clean-room implementation (zero third-party libraries, no Docker SDK, no runc wrappers)

---

## Executive Overview

**Kapsule** is an educational, production-grade mini container runtime built from the ground up in pure Go. It proves a foundational thesis of cloud-native computing:

> *"Containers are not physical or virtual machines. Containers do not exist as distinct objects inside the Linux kernel. A container is simply a standard Linux process isolated by **Namespaces**, governed by **Control Groups (cgroups)**, and confined to an isolated Virtual Filesystem via **`pivot_root(2)`**."*

Every modern container engine—whether **Docker**, **Podman**, **CRI-O**, or **containerd**—relies on an Open Container Initiative (OCI) runtime like `runc` or `crun`. Underneath thousands of lines of abstractions, `runc` executes the exact sequence of kernel syscalls implemented in this project.

This repository is designed as an enterprise portfolio piece demonstrating low-level systems programming proficiency and showing how kernel-level mechanics directly inform enterprise troubleshooting in **Kubernetes** and **Red Hat OpenShift** environments (tailored for IBM Cloud Software Engineering roles).

---

## Architecture Diagram

The lifecycle of a Kapsule container follows a deterministic, race-condition-free initialization pipeline:

```
[ Host Shell ]
      │
      ▼  sudo ./kapsule run ./rootfs /bin/sh
┌────────────────────────────────────────────────────────────────────────┐
│  KAPSULE PARENT PROCESS (Host PID: 12480, Host Namespaces)             │
│                                                                        │
│  1. Verifies CAP_SYS_ADMIN (root privileges)                           │
│  2. Creates anonymous synchronization OS pipe (syncReader / syncWriter)│
│  3. Formulates Re-Exec command: /proc/self/exe child <rootfs> <cmd>    │
│  4. Configures SysProcAttr.Cloneflags:                                 │
│     ├── CLONE_NEWUTS  (Isolate Hostname & Domain)                      │
│     ├── CLONE_NEWPID  (Isolate Process Tree - Child becomes PID 1)    │
│     └── CLONE_NEWNS   (Isolate Mount Table / VFS)                      │
│  5. cmd.Start() -> Forks Child Process                                 │
└────────────────────────────────────┬───────────────────────────────────┘
                                     │
                 ┌───────────────────┴───────────────────┐
                 │                                       │
                 ▼ (Host PID: 12481)                     ▼
┌──────────────────────────────────────┐  ┌──────────────────────────────┐
│ KAPSULE PARENT (Supervisor)          │  │ KAPSULE CHILD PROCESS        │
│                                      │  │ (Virtual PID: 1 inside NS)   │
│ 1. Attaches Host PID 12481 to        │  │                              │
│    /sys/fs/cgroup/kapsule/cgroup.procs│  │ 1. Blocks reading 1 byte     │
│ 2. Enforces Cgroup v2 limits:        │  │    from sync pipe (FD 3)     │
│    ├── pids.max = 50 (Fork bomb lock)│  │    [Wait for Cgroup setup]   │
│    ├── memory.max = 100MB            │  │                              │
│    └── cpu.max = 50ms / 100ms quota  │  │                              │
│ 3. Writes 1 byte to sync pipe ───────┼─>│ 2. Unblocks!                 │
│ 4. Waits on cmd.Wait()...            │  │ 3. sethostname("kapsule")    │
│                                      │  │ 4. securePivotRoot(rootfs):  │
│                                      │  │    - MS_PRIVATE propagation  │
│                                      │  │    - bind-mount rootfs       │
│                                      │  │    - pivot_root(new, put_old)│
│                                      │  │    - chdir("/")              │
│                                      │  │    - umount2(old, MNT_DETACH)│
│                                      │  │ 5. mount("proc", "/proc")    │
│                                      │  │ 6. syscall.Exec(binary)      │
│                                      │  └──────────────┬───────────────┘
│                                      │                 │
│                                      │                 ▼
│                                      │  ┌──────────────────────────────┐
│                                      │  │ TARGET WORKLOAD (/bin/sh)    │
│                                      │  │ True PID 1, Clean VFS,       │
│                                      │  │ Host Completely Inaccessible │
│                                      │  └──────────────┬───────────────┘
│                                      │                 │
│                                      │                 ▼ (Exit)
│ 5. Catches child exit code           │<────────────────┘
│ 6. Cleans up /sys/fs/cgroup/kapsule  │
│ 7. os.Exit(childExitCode)            │
└──────────────────────────────────────┘
```

---

## Core Mechanisms Implemented

### 1. Linux Namespaces: Kernel Resource Virtualization

Linux namespaces wrap global system resources into isolated abstractions. Processes within a namespace believe they possess a dedicated instance of that system resource.

| Namespace | Clone Flag | Isolated Resource | Implementation in Kapsule |
| :--- | :--- | :--- | :--- |
| **UTS** | `CLONE_NEWUTS` | Hostname and NIS Domain Name | Calls `syscall.Sethostname([]byte("kapsule"))`. Changing hostname inside container has zero effect on host. |
| **PID** | `CLONE_NEWPID` | Process IDs & Process Hierarchy | The first child process spawned inside the namespace receives PID 1. Processes inside cannot see, inspect, or signal processes in the parent namespace. |
| **Mount** | `CLONE_NEWNS` | Virtual Filesystem (VFS) Mount Points | Mounts and unmounts (such as `/proc` or `pivot_root`) are private to this container, preventing host mount table pollution. |

---

### 2. The "Re-Exec" Pattern: Overcoming Go Runtime Concurrency

A classic problem in writing container runtimes in Go is the interaction between Go’s runtime and Linux namespace system calls:

#### Why can't we just call `syscall.Unshare()` inside Go?
1. **The M:N Threading Model:** The Go runtime manages user-space goroutines (`G`) across a pool of operating system threads (`M`) scheduled by logical processors (`P`). In Linux, namespaces are per-OS-thread attributes. If a goroutine calls `unshare(2)` or `setns(2)`, only that specific OS thread enters the namespace. If the Go scheduler subsequently switches that goroutine to a different OS thread `M`, the process abruptly falls back into the host namespace!
2. **PID 1 Guarantee:** When a process invokes `clone(CLONE_NEWPID)` or `unshare(CLONE_NEWPID)`, the calling process does **not** move into the new PID namespace; only **subsequent children** do. Thus, the container supervisor itself cannot become PID 1 without forking.

#### The Kapsule Re-Exec Solution:
1. When the user executes `kapsule run <rootfs> <command>`, Kapsule executes its parent supervisor logic.
2. The parent re-invokes itself using `/proc/self/exe child <rootfs> <command>` via `exec.Command`.
3. In `cmd.SysProcAttr`, we pass `Cloneflags: CLONE_NEWUTS | CLONE_NEWPID | CLONE_NEWNS`.
4. When the Linux kernel handles `clone(2)`, it starts the newly spawned binary directly inside the target namespaces.
5. The child begins life as **PID 1**, running clean, single-threaded Go initialization before executing the user's workload.

---

### 3. Filesystem Isolation: Why `pivot_root(2)` and NOT `chroot(2)`

One of the most critical security distinctions in container engineering is between `chroot(2)` and `pivot_root(2)`. Many naive container tutorials use `chroot`. **Kapsule uses `pivot_root`.**

```
                     ┌────────────────────────────────────────┐
                     │          The chroot(2) Flaw            │
                     └────────────────────────────────────────┘
  Host Root (/) ─────────────────────────────────────────────────────────────
         │                                                            ▲
         ├── bin/                                                     │ [Trivial Jailbreak via fchdir]
         ├── etc/                                                     │ 
         └── jail/  <── chroot() sets current->fs->root here          │
               ├── bin/                                               │
               └── [Malicious code opens outside fd, chroots, escapes]┘
```

```
                     ┌────────────────────────────────────────┐
                     │       The pivot_root(2) Guarantee      │
                     └────────────────────────────────────────┘
  1. Bind-mount new rootfs to itself (becomes distinct VFS mount point).
  2. pivot_root(new_root, put_old) swaps mount points at VFS level:
     - "/" becomes new_root
     - Old host root is moved to "/.pivot_old"
  3. umount2("/.pivot_old", MNT_DETACH) lazy-unmounts old root.
  4. Result: Host VFS tree is COMPLETELY DETACHED from mount namespace table.
     Host files are physically inaccessible, even to UID 0!
```

#### Detailed Security Analysis:
* **The Vulnerability of `chroot(2)`:**
  `chroot` merely modifies the `root` path pointer in the process's kernel `task_struct->fs->root`. It **does not alter the VFS mount table**. Any process with `CAP_SYS_CHROOT` can trivially break out:
  ```c
  int fd = open(".", O_RDONLY);
  chroot("empty_subfolder");
  fchdir(fd);
  chroot("../../../../../"); // ESCAPED BACK TO HOST ROOT!
  ```
* **The Fortress of `pivot_root(2)`:**
  `pivot_root` changes the root mount point of the caller's mount namespace. After swapping mounts, Kapsule calls:
  ```go
  syscall.Unmount("/.pivot_old", syscall.MNT_DETACH)
  ```
  `MNT_DETACH` permanently severs the old host root filesystem from the container's mount tree. There is no relative or absolute path, no open descriptor, and no syscall that can traverse back to the host filesystem.

---

### 4. Control Groups (Cgroups v2): Resource Governance & Fork Bomb Mitigation

Namespaces provide **isolation** (what you can see), but they provide zero **resource limitation** (what you can use). Without Cgroups, a containerized process can consume 100% of the host's CPU, trigger kernel Out-Of-Memory (OOM) panics, or execute a fork bomb that freezes the entire operating system.

Kapsule targets modern **Cgroups v2** (unified hierarchy located at `/sys/fs/cgroup`).

#### Race-Condition-Free Initialization via Synchronization Pipe:
If the child process immediately executed user code before the parent attached it to a cgroup, a fast fork bomb could exhaust system PIDs before limits were applied.

Kapsule solves this using a kernel pipe barrier:
1. Parent creates an `os.Pipe()` and passes the reader to the child via `cmd.ExtraFiles` (accessible at FD 3).
2. Child blocks on `syncPipe.Read()`.
3. Parent creates `/sys/fs/cgroup/kapsule`, writes the child's host PID to `cgroup.procs`, and configures:
   * **`pids.max = 50`**: Hard limit on active tasks. Prevents fork bombs (`:(){ :|:& };:`). The 51st fork fails with `EAGAIN` ("Resource temporarily unavailable").
   * **`memory.max = 104857600` (100 MB)**: Limits resident memory. Exceeding this triggers the cgroup OOM killer without affecting the host.
   * **`cpu.max = 50000 100000`**: Restricts CPU bandwidth to 50ms per 100ms CFS period (50% of one core).
4. Parent writes 1 byte to the pipe, releasing the child to execute the container workload.
5. On container termination, parent cleans up `/sys/fs/cgroup/kapsule`.

---

## Enterprise Translation: Debugging Kubernetes & Red Hat OpenShift Workloads

*(Tailored for IBM Cloud Software Engineering & Red Hat OpenShift Roles)*

Understanding how containers operate at the raw syscall and kernel level transforms high-level Kubernetes symptoms into clear, actionable root-cause investigations. Here is how Kapsule's architecture directly maps to enterprise OpenShift and Kubernetes engineering:

### 1. Diagnosing `CrashLoopBackOff` & OOMKilled (Exit Code 137)
* **The High-Level Symptom:** In OpenShift or Kubernetes, a pod crashes with `OOMKilled` (Exit Code `137` = `128 + SIGKILL (9)`).
* **The Kernel Reality:** The pod's `resources.limits.memory` manifest is translated by the Kubelet and CRI-O into Cgroup v2 `memory.max` (or Cgroup v1 `memory.limit_in_bytes`). When the container's working set memory hits this ceiling, the Linux kernel's Memory Management subsystem invokes the Cgroup OOM Killer to terminate processes in that specific leaf cgroup.
* **Senior Engineering Debug Strategy:**
  Instead of guessing with `kubectl top` (which samples metrics at coarse 1-minute intervals and misses microsecond spikes), inspect the kernel cgroup events directly:
  ```bash
  # Inside Red Hat Enterprise Linux CoreOS (RHCOS) node:
  cat /sys/fs/cgroup/kubepods.slice/.../memory.events
  ```
  Look for:
  * `oom`: The number of times the cgroup entered the OOM memory state.
  * `oom_kill`: The exact count of processes terminated by the kernel.
  * `high`: Indicates memory throttle reclaim pressure before the kill.

---

### 2. PID Starvation, Node Freezes, and `pids.max`
* **The High-Level Symptom:** A rogue pod with runaway threads locks up a Kubernetes worker node. Neither `kubelet`, `sshd`, nor monitoring agents can execute commands. Node transitions to `NotReady`.
* **The Kernel Reality:** By default, Linux has a global PID limit (`/proc/sys/kernel/pid_max`, typically 32,768 or 4,194,304). If containers are not bounded by `pids.max`, a thread leak or fork bomb exhausts the global PID table. The kernel cannot allocate a new `task_struct` for any process on the node.
* **Senior Engineering Debug Strategy:**
  Kubernetes introduced PID limiting (`SupportNodePidsLimit`), and OpenShift enforces it via `KubeletConfig` custom resources:
  ```yaml
  apiVersion: machineconfiguration.openshift.io/v1
  kind: KubeletConfig
  metadata:
    name: pod-pid-limits
  spec:
    kubeletConfig:
      podPidsLimit: 4096
  ```
  In Kapsule, we demonstrate this exact mechanism with `pids.max = 50`. When a fork bomb runs, the host remains completely smooth, and only the offending container receives `EAGAIN`.

---

### 3. Zombie Processes and PID 1 Signal Handling
* **The High-Level Symptom:** A container runs fine for days, then crashes or stops accepting connections. `ps aux` reveals hundreds of `[defunct]` processes.
* **The Kernel Reality:**
  In Linux, PID 1 has two unique, non-delegable responsibilities:
  1. **Adopt and Reap Orphans:** When a parent process terminates before its child, the child is re-parented to PID 1. When that child exits, PID 1 must invoke `wait()` or `waitpid()` to clear the child's entry from the kernel process table. If PID 1 ignores `SIGCHLD`, the process remains a zombie (`[defunct]`).
  2. **Default Signal Handling:** The kernel treats PID 1 specially; signals like `SIGTERM` and `SIGINT` do not have default terminate actions unless PID 1 explicitly installs a signal handler!
* **Senior Engineering Debug Strategy:**
  This is why running Node.js, Python, or shell scripts directly as the entrypoint in a Dockerfile/Containerfile can cause problems. In Kubernetes/OpenShift, container specs should use `shareProcessNamespace: true` or lightweight init systems (like `tini` or `dumb-init`) to ensure zombie reaping and proper `SIGTERM` graceful shutdowns.

---

### 4. Container Escapes & OpenShift Security Context Constraints (SCCs)
* **The High-Level Symptom:** Security teams flag workloads demanding `privileged: true` or `hostPath` mounts.
* **The Kernel Reality:**
  As demonstrated in Kapsule:
  * Running a container without `CLONE_NEWNS` or with improper mount propagation leaves host mounts exposed.
  * Running with `CAP_SYS_ADMIN` allows a container to remount filesystems, disable cgroups, or invoke `setns(2)` to escape into host namespaces.
  * Using `chroot` instead of `pivot_root` allows trivial jailbreaks via open directory descriptors.
* **Senior Engineering Debug Strategy:**
  Red Hat OpenShift implements **Security Context Constraints (SCCs)** (the precursor to Kubernetes Pod Security Standards). OpenShift's default `restricted-v2` SCC:
  * Prohibits `privileged` mode (blocking `CAP_SYS_ADMIN`).
  * Disallows `hostPath` volume mounts (guaranteeing `pivot_root` boundaries).
  * Enforces unprivileged UID allocation via kernel User Namespaces (`CLONE_NEWUSER`).

---

### 5. The OCI & Container Runtime Architecture Stack

```
┌────────────────────────────────────────────────────────┐
│                   Kubernetes Master                    │
└───────────────────────────┬────────────────────────────┘
                            │ API Request (Create Pod)
                            ▼
┌────────────────────────────────────────────────────────┐
│               Kubelet (Worker Node Daemon)             │
└───────────────────────────┬────────────────────────────┘
                            │ gRPC (Container Runtime Interface / CRI)
                            ▼
┌────────────────────────────────────────────────────────┐
│           CRI-O / containerd (CRI Daemon)              │
└───────────────────────────┬────────────────────────────┘
                            │ OCI Specification (config.json + rootfs)
                            ▼
┌────────────────────────────────────────────────────────┐
│             runc / crun (OCI Runtime)                  │
│       ===> [ EXACTLY WHAT KAPSULE IMPLEMENTS ] <===     │
└───────────────────────────┬────────────────────────────┘
                            │ Direct Linux Syscalls:
                            │ - clone(CLONE_NEWUTS | CLONE_NEWPID | CLONE_NEWNS)
                            │ - write(/sys/fs/cgroup/pids.max)
                            │ - pivot_root(rootfs, put_old)
                            │ - mount("proc", "/proc", "proc")
                            │ - execve(binary)
                            ▼
┌────────────────────────────────────────────────────────┐
│                     Linux Kernel                       │
└────────────────────────────────────────────────────────┘
```

When an engineer understands Kapsule, they understand what happens at the exact bottom of the Kubernetes and OpenShift execution stack.

---

## Quickstart & Verification Guide

### Prerequisites
* A 64-bit Linux distribution (Ubuntu 20.04+, RHEL 8+, Fedora, Arch, or WSL2 with Cgroups v2 enabled).
* Go 1.20+ installed.
* Standard utilities: `make`, `curl`, `tar`, `sudo`.

---

### Step 1: Clone & Setup
```bash
# Navigate to the project directory
cd kapsule

# Download the minimal Alpine Linux root filesystem (rootfs)
make rootfs
```
*(The setup script detects your host CPU architecture and downloads the official Alpine mini-rootfs into `./rootfs`.)*

---

### Step 2: Build the Runtime
```bash
make build
```
*(Compiles a pure, static Go binary named `kapsule` using `CGO_ENABLED=0`.)*

---

### Step 3: Run an Interactive Container Shell
```bash
make run
```
You are now inside an isolated container shell!
Verify your environment:
```sh
# Verify you are PID 1:
ps aux
# Output:
# PID   USER     TIME  COMMAND
#   1 root      0:00 /bin/sh
#   2 root      0:00 ps aux

# Verify your isolated hostname:
hostname
# Output: kapsule

# Check your root filesystem:
ls -la /
# Notice that only the Alpine rootfs exists. Host root is completely detached.

# Exit container:
exit
```

---

### Step 4: Verify Cgroups v2 Fork Bomb Immunity
In another terminal or via the Makefile, run the fork bomb verification test:
```bash
make test-forkbomb
```
The test attempts to spawn 100 background processes in a loop. Because Kapsule configures `pids.max = 50`:
```
-sh: can't fork: Resource temporarily unavailable
```
The kernel halts runaway process generation immediately at 50 tasks. The host system remains 100% responsive and healthy!

---

## Technical Interview Cheatsheet: Container Systems

When discussing container internals during systems engineering interviews, reference these key points:

| Interview Question | Junior Answer | Senior / Staff Answer (The Kapsule Perspective) |
| :--- | :--- | :--- |
| **"What is a container?"** | "A lightweight virtual machine with an image." | "A container is a regular Linux process running with unshared namespaces (for isolation), bounded by cgroups (for resource quotas), and confined to a pivot_root mount tree." |
| **"Why is `pivot_root` used instead of `chroot`?"** | "`pivot_root` is newer and better." | "`chroot` only changes the process path resolution pointer and is vulnerable to `fchdir` jailbreaks. `pivot_root` moves mount points in the VFS mount namespace; combined with `MNT_DETACH`, it completely unmounts and severs the host root filesystem." |
| **"Why do Go container runtimes use the re-exec pattern?"** | "To run the command again." | "Go's multi-threaded M:N runtime scheduler conflicts with per-thread Linux namespace syscalls like `setns`/`unshare`. Re-executing `/proc/self/exe` ensures clean, deterministic, single-threaded startup as true PID 1." |
| **"How does `pids.max` protect Kubernetes nodes?"** | "It limits pods." | "It bounds the tasks in a Cgroup v2 leaf slice, preventing any single container from exhausting the global Linux PID table (`/proc/sys/kernel/pid_max`) and rendering the host node unresponsive." |

---

## File Structure

```
kapsule/
├── main.go        # Complete container runtime engine (Namespaces, Re-exec, pivot_root, Cgroups v2)
├── setup.sh       # Automated multi-arch Alpine Linux rootfs download & extraction script
├── Makefile       # Build, test, and container execution automation
├── go.mod         # Go module definition (pure standard library)
└── README.md      # Enterprise architecture whitepaper & engineering documentation
```

---

## Conclusion

Kapsule strips away the layers of tooling surrounding modern containers, presenting clean, auditable systems code that directly speaks the language of the Linux kernel.

Whether you are developing next-generation distributed systems or diagnosing elusive Kubernetes production incidents on Red Hat OpenShift, mastery of these low-level primitives is the hallmark of a Senior Cloud Systems Engineer.
