# Kapsule: Complete Project Reference & Line-by-Line Technical Manual

**Repository:** [github.com/ireneyk/kapsule](https://github.com/ireneyk/kapsule)  
**Language:** Go (Pure Standard Library & Syscall, Zero Third-Party Packages)  

---

## 1. Project Overview & Motivation

### What is Kapsule?
Kapsule is a lightweight, dependency-free Linux container runtime written from scratch in Go. It does not use Docker, containerd, `runc`, or any third-party SDKs. It uses raw Linux kernel system calls to turn a standard process into an isolated, secure container.

### The Big Idea
Many engineers think containers are virtual machines or physical boxes inside Linux. In reality, **there is no such thing as a "container" object in the Linux kernel source code**. 

A container is just a normal Linux process (`struct task_struct`) constrained by three core kernel primitives:
1. **Namespaces:** What the process is allowed to **see** (its own hostname, its own isolated process table where it is PID 1, and its own filesystem mounts).
2. **Control Groups (Cgroups v2):** What the process is allowed to **use** (hardware resource limits: max 50 processes to stop fork bombs, max 100MB RAM, max 50% CPU).
3. **`pivot_root`:** Where the process is allowed to **live** (swaps the host filesystem for an isolated Alpine rootfs and completely cuts off the host filesystem so the process cannot escape).

---

## 2. Repository File Structure

The project repository contains the following clean, minimal files:

```
kapsule/
├── .gitignore          # Ignores compiled binaries, rootfs directories, and caches
├── LICENSE             # MIT License
├── Makefile            # Automation commands (build, run, test)
├── README.md           # Project documentation and architectural overview
├── go.mod              # Go module definition (pure Go 1.22, zero external dependencies)
├── main.go             # Complete container runtime implementation (~317 lines)
├── setup.sh            # Helper script to download & unpack Alpine Linux rootfs
└── PROJECT_EXPLANATION.md # This comprehensive technical reference document
```

---

## 3. Every Single File Explained

### 3.1 `go.mod`
* **Purpose:** Declares the Go module name and version.
* **Contents:**
  ```go
  module kapsule

  go 1.22
  ```
* **Why it matters:** It proves that Kapsule uses **zero external dependencies**. No third-party packages, no Docker libraries. Everything is built with standard library packages (`os`, `os/exec`, `syscall`, `filepath`, `log`).

---

### 3.2 `.gitignore`
* **Purpose:** Keeps the Git repository clean by preventing large binaries and temporary directories from being committed.
* **Ignored items:**
  - `kapsule` and `*.exe` (compiled binaries)
  - `rootfs/` (the unpacked Alpine Linux filesystem)
  - `.cache/` and `*.tar.gz` (cached rootfs downloads)
  - `.vscode/`, `.idea/`, `.DS_Store` (editor metadata)

---

### 3.3 `LICENSE`
* **Purpose:** MIT open-source license protecting and attributing the work to Irene.

---

### 3.4 `setup.sh` (Rootfs Downloader)
* **Purpose:** Automatically downloads and unpacks a minimal Alpine Linux root filesystem (rootfs) so Kapsule has an isolated OS to run inside.
* **How it works step by step:**
  1. Checks for required host utilities (`curl` and `tar`).
  2. Detects the machine's CPU architecture (`uname -m` -> `x86_64` or `aarch64`).
  3. Downloads the official `alpine-minirootfs-3.20.0` tarball from the Alpine CDN into a local `.cache/` folder.
  4. Unpacks the tarball into `./rootfs`.
  5. Copies `/etc/resolv.conf` into `./rootfs/etc/resolv.conf` so the container has working DNS for network requests.

---

### 3.5 `Makefile` (Build & Automation)
* **Purpose:** Provides simple, 1-word commands to build and test the project.
* **Targets:**
  - `make build`: Compiles `main.go` into a stripped, static Linux binary using `CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o kapsule .`.
  - `make rootfs`: Runs `setup.sh` to get the rootfs ready.
  - `make run`: Starts an interactive shell inside Kapsule (`sudo ./kapsule run ./rootfs /bin/sh`).
  - `make test-pid`: Runs `ps aux` inside the container to verify it runs as PID 1.
  - `make test-hostname`: Verifies the hostname inside the container is isolated as `kapsule`.
  - `make test-forkbomb`: Attempts to spawn 100 processes to verify Cgroups v2 blocks it at 50 tasks.
  - `make clean`: Removes the compiled binary, rootfs, and downloaded caches.

---

## 4. Line-by-Line Breakdown of `main.go`

`main.go` contains the entire container runtime. Below is an exhaustive, section-by-section and line-by-line explanation.

```go
1: //go:build linux
2: // +build linux
```
* **Explanation:** These build tags tell the Go compiler to only compile this file on Linux. Because Kapsule directly calls Linux-specific system calls (`syscall.CLONE_NEWPID`, `syscall.PivotRoot`, etc.), it cannot compile on native Windows or macOS.

```go
4: package main
5: 
6: import (
7: 	"fmt"
8: 	"log"
9: 	"os"
10: 	"os/exec"
11: 	"path/filepath"
12: 	"strconv"
13: 	"syscall"
14: )
```
* **Explanation:** Standard library imports only.
  * `fmt`: String formatting and printing usage instructions.
  * `log`: Logging container status messages with `[kapsule]` prefix.
  * `os`: File operations, pipes, environment variables, exit codes.
  * `os/exec`: Spawning the supervisor child process.
  * `path/filepath`: Resolving absolute paths for the root filesystem.
  * `strconv`: Converting integers to strings (e.g. child PID for Cgroups).
  * `syscall`: Raw low-level Linux kernel system calls (`Cloneflags`, `Mount`, `PivotRoot`, `Exec`, `Sethostname`).

```go
16: // Default limits for the container (Cgroups v2)
17: const (
18: 	cgroupPath     = "/sys/fs/cgroup/kapsule"
19: 	maxProcesses   = "50"           // pids.max: limits container to 50 processes to stop fork bombs
20: 	maxMemoryBytes = "104857600"    // memory.max: hard cap at 100MB RAM
21: 	cpuQuota       = "50000 100000" // cpu.max: 50ms per 100ms window = 50% of one CPU core
22: )
```
* **Explanation:** Configuration for Cgroups v2 resource limits:
  * `cgroupPath`: The slice created in the unified Linux Cgroups v2 hierarchy (`/sys/fs/cgroup/kapsule`).
  * `maxProcesses`: Set to 50. If any app or fork bomb inside the container tries to create process #51, the Linux kernel rejects it.
  * `maxMemoryBytes`: 104,857,600 bytes = 100 Megabytes. If memory exceeds 100MB, the Linux kernel OOM killer triggers inside the container.
  * `cpuQuota`: `50000 100000` means 50,000 microseconds quota per 100,000 microseconds period. This throttles the container to at most 50% of 1 CPU core.

```go
24: func main() {
25: 	log.SetFlags(0)
26: 	log.SetPrefix("[kapsule] ")
27: 
28: 	if len(os.Args) < 2 {
29: 		printUsage()
30: 		os.Exit(1)
31: 	}
```
* **Explanation:** Configures clean log output without timestamps, just `[kapsule] `, and checks that at least one argument was passed.

```go
33: 	switch os.Args[1] {
34: 	case "run":
35: 		if len(os.Args) < 4 {
36: 			log.Println("error: missing arguments")
37: 			printUsage()
38: 			os.Exit(1)
39: 		}
40: 		runParent(os.Args[2], os.Args[3], os.Args[3:])
```
* **Explanation:** The primary command invoked by the user: `kapsule run <rootfs> <command> [args...]`. It calls `runParent()`.

```go
43: 	case "child":
44: 		if len(os.Args) < 4 {
45: 			log.Fatal("fatal: invalid child call")
46: 		}
47: 		runChild(os.Args[2], os.Args[3], os.Args[3:])
```
* **Explanation:** Internal command. The user never types `child`. The parent process calls itself with `child` via `/proc/self/exe` after creating new namespaces.

```go
50: 	case "help", "--help", "-h":
51: 		printUsage()
52: 
53: 	default:
54: 		log.Printf("unknown command: %s\n", os.Args[1])
55: 		printUsage()
56: 		os.Exit(1)
57: 	}
58: }
```
* **Explanation:** Handles help and invalid commands.

---

### Deep Dive: `runParent()` (Lines 60–142)

```go
63: func runParent(rootfs string, cmd string, cmdArgs []string) {
64: 	if os.Geteuid() != 0 {
65: 		log.Fatal("kapsule needs root privileges (run with sudo)")
66: 	}
```
* **Explanation:** Checks if the caller is root (`euid == 0`). Linux namespaces and writing to `/sys/fs/cgroup` require `CAP_SYS_ADMIN` capability.

```go
69: 	absRootfs, err := filepath.Abs(rootfs)
70: 	if err != nil {
71: 		log.Fatalf("invalid rootfs path %q: %v", rootfs, err)
72: 	}
73: 	if info, err := os.Stat(absRootfs); err != nil || !info.IsDir() {
74: 		log.Fatalf("rootfs directory %q not found", absRootfs)
75: 	}
```
* **Explanation:** Resolves the rootfs path to an absolute path and verifies the directory exists before proceeding.

```go
80: 	syncReader, syncWriter, err := os.Pipe()
81: 	if err != nil {
82: 		log.Fatalf("failed to create sync pipe: %v", err)
83: 	}
84: 	defer syncWriter.Close()
```
* **Explanation:** Creates an anonymous pipe for synchronization between parent and child.
  * **Why is this critical?** Without this, there is a race condition: the child process might start executing user code *before* the parent has finished writing the Cgroup limits. If the user command is a fork bomb, it could crash the system before limits are applied! The pipe forces the child to pause until the parent says "limits are ready, go ahead."

```go
86: 	// Re-exec pattern:
87: 	// Why re-exec /proc/self/exe?
88: 	// In Go, the runtime spawns multiple OS threads behind the scenes.
89: 	// Linux namespaces apply per-thread, so modifying them in a running Go process can desync threads.
90: 	// Re-running ourselves through /proc/self/exe gives us a clean, single-threaded startup inside the new namespaces.
91: 	childArgs := append([]string{"child", absRootfs}, cmdArgs...)
92: 	cmdExec := exec.Command("/proc/self/exe", childArgs...)
93: 
94: 	cmdExec.Stdin = os.Stdin
95: 	cmdExec.Stdout = os.Stdout
96: 	cmdExec.Stderr = os.Stderr
97: 
98: 	cmdExec.ExtraFiles = []*os.File{syncReader}
```
* **Explanation:**
  * Uses `/proc/self/exe` to re-execute the current executable with argument `child`.
  * Connects standard input, output, and error so interactive terminal sessions work.
  * `cmdExec.ExtraFiles = []*os.File{syncReader}`: Passes the read end of our sync pipe to the child. In Unix, extra files passed to a child start at file descriptor 3 (`uintptr(3)`).

```go
105: 	cmdExec.SysProcAttr = &syscall.SysProcAttr{
106: 		Cloneflags: syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
107: 	}
```
* **Explanation:** The heart of Linux namespace isolation:
  * `CLONE_NEWUTS`: Isolates hostname and domain name.
  * `CLONE_NEWPID`: Isolates the process ID tree. The new process will be PID 1 inside the container.
  * `CLONE_NEWNS`: Isolates the VFS mount namespace. Mounts performed inside the container will never leak to the host.

```go
109: 	log.Println("Starting container with UTS, PID, and Mount namespaces...")
110: 	if err := cmdExec.Start(); err != nil {
111: 		syncReader.Close()
112: 		log.Fatalf("failed to start container: %v", err)
113: 	}
114: 
115: 	syncReader.Close()
```
* **Explanation:** Calls `Start()`, which executes the `clone()` system call with the flags above. The parent closes its copy of `syncReader`.

```go
118: 	childPid := cmdExec.Process.Pid
119: 	log.Printf("Container started (Host PID: %d, Container PID: 1)", childPid)
120: 
121: 	if err := setupCgroups(childPid); err != nil {
122: 		log.Printf("cgroup warning: %v", err)
123: 	}
```
* **Explanation:** Obtains the child process ID on the host, prints it, and calls `setupCgroups(childPid)` to place it under CPU, memory, and process limits.

```go
125: 	if _, err := syncWriter.Write([]byte{1}); err != nil {
126: 		log.Fatalf("failed to signal child process: %v", err)
127: 	}
128: 	syncWriter.Close()
129: 
130: 	defer cleanupCgroups()
```
* **Explanation:** Writes 1 byte to the sync pipe to unblock the child. Then closes the writer and registers `cleanupCgroups()` to clean up the cgroup directory when the container finishes.

```go
133: 	if err := cmdExec.Wait(); err != nil {
134: 		if exitErr, ok := err.(*exec.ExitError); ok {
135: 			os.Exit(exitErr.ExitCode())
136: 		}
137: 		log.Fatalf("container error: %v", err)
138: 	}
139: 
140: 	log.Println("Container exited cleanly.")
141: }
```
* **Explanation:** The parent waits for the child process to complete. If the child exits with an error code (e.g. 137 or 1), the parent passes that exact exit code back to the host shell.

---

### Deep Dive: `runChild()` (Lines 144–189)

```go
145: func runChild(rootfs string, cmd string, cmdArgs []string) {
146: 	syncPipe := os.NewFile(uintptr(3), "sync-pipe")
147: 	if syncPipe != nil {
148: 		barrier := make([]byte, 1)
149: 		_, _ = syncPipe.Read(barrier)
150: 		syncPipe.Close()
151: 	}
```
* **Explanation:** Accesses file descriptor 3 (passed from parent). Calls `syncPipe.Read(barrier)`. This blocks execution until the parent writes the 1 byte, guaranteeing Cgroup limits are applied before running code.

```go
155: 	if err := syscall.Sethostname([]byte("kapsule")); err != nil {
156: 		log.Fatalf("sethostname failed: %v", err)
157: 	}
```
* **Explanation:** Sets the container's private hostname to `kapsule`. Because of `CLONE_NEWUTS`, the host's real hostname remains untouched.

```go
160: 	if err := isolateFilesystem(rootfs); err != nil {
161: 		log.Fatalf("pivot_root failed: %v", err)
162: 	}
```
* **Explanation:** Calls `isolateFilesystem()` to securely swap the root filesystem to Alpine using `pivot_root`.

```go
166: 	if err := os.MkdirAll("/proc", 0755); err != nil && !os.IsExist(err) {
167: 		log.Fatalf("mkdir /proc failed: %v", err)
168: 	}
169: 	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
170: 		log.Fatalf("mount /proc failed: %v", err)
171: 	}
```
* **Explanation:** Mounts a fresh, private `procfs` at `/proc`.
  * **Why is this necessary?** Standard Linux commands like `ps`, `top`, and `kill` read from `/proc` to inspect processes. Without mounting a fresh `/proc` inside the new PID namespace, `ps` would still read the host's `/proc` and display host processes! Mounting fresh `proc` ensures tools inside the container only see container processes.

```go
174: 	env := []string{
175: 		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
176: 		"TERM=xterm-256color",
177: 		"HOME=/root",
178: 		"USER=root",
179: 		"CONTAINER=kapsule",
180: 	}
181: 
182: 	target := findBinary(cmd)
```
* **Explanation:** Strips host environment variables (preventing secret leakage) and provides clean defaults. Resolves the executable path.

```go
186: 	if err := syscall.Exec(target, cmdArgs, env); err != nil {
187: 		log.Fatalf("execve(%q) failed: %v", target, err)
188: 	}
189: }
```
* **Explanation:** Invokes `execve(2)`. This **replaces** the Kapsule Go binary entirely in memory with the target user binary (e.g. `/bin/sh` or `/bin/ps`). The target binary becomes true PID 1, and the Go runtime completely drops out of memory.

---

### Deep Dive: `isolateFilesystem()` (Lines 191–234)

```go
198: func isolateFilesystem(newRoot string) error {
199: 	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
200: 		return fmt.Errorf("make mount private failed: %w", err)
201: 	}
```
* **Explanation:** Recursively sets all mounts to `MS_PRIVATE`. By default in systemd, mounts are shared (`MS_SHARED`). If you mount or unmount something in a shared mount, it propagates back to the host! Making it private stops all propagation.

```go
204: 	if err := syscall.Mount(newRoot, newRoot, "bind", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
205: 		return fmt.Errorf("bind-mount rootfs failed: %w", err)
206: 	}
```
* **Explanation:** A strict Linux kernel requirement for `pivot_root`: the new root directory **must be an active mount point**. Bind-mounting `newRoot` onto itself satisfies this requirement.

```go
210: 	putOld := filepath.Join(newRoot, ".oldroot")
211: 	if err := os.MkdirAll(putOld, 0700); err != nil {
212: 		return fmt.Errorf("create .oldroot failed: %w", err)
213: 	}
```
* **Explanation:** Creates a temporary holding directory `/.oldroot` where the old host root will be parked during the pivot.

```go
216: 	if err := syscall.PivotRoot(newRoot, putOld); err != nil {
217: 		return fmt.Errorf("syscall pivot_root failed: %w", err)
218: 	}
```
* **Explanation:** Atomic pivot:
  * `newRoot` becomes the new `/`.
  * The previous host `/` is moved to `/.oldroot`.

```go
220: 	if err := syscall.Chdir("/"); err != nil {
221: 		return fmt.Errorf("chdir / failed: %w", err)
222: 	}
```
* **Explanation:** Moves working directory to the new `/`.

```go
225: 	if err := syscall.Unmount("/.oldroot", syscall.MNT_DETACH); err != nil {
226: 		return fmt.Errorf("unmount old root failed: %w", err)
227: 	}
228: 
229: 	_ = os.Remove("/.oldroot")
230: 	return nil
231: }
```
* **Explanation:** Unmounts `/.oldroot` using `MNT_DETACH` (lazy unmount) and deletes the folder.
  * **Why `pivot_root` instead of `chroot`?** `chroot` only changes the path string pointer. An attacker with root can escape a `chroot` by opening a file descriptor to `..` and calling `fchdir()`. With `pivot_root` + `MNT_DETACH`, the host filesystem is completely removed from the mount table. There is nothing to escape into!

---

### Deep Dive: `setupCgroups()` & `cleanupCgroups()` (Lines 236–277)

```go
239: func setupCgroups(pid int) error {
240: 	if _, err := os.Stat("/sys/fs/cgroup"); os.IsNotExist(err) {
241: 		return fmt.Errorf("cgroups v2 not available")
242: 	}
243: 
244: 	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
245: 		return fmt.Errorf("mkdir cgroup failed: %w", err)
246: 	}
```
* **Explanation:** Verifies unified Cgroups v2 exists and creates `/sys/fs/cgroup/kapsule`.

```go
249: 	procsFile := filepath.Join(cgroupPath, "cgroup.procs")
250: 	if err := os.WriteFile(procsFile, []byte(strconv.Itoa(pid)), 0644); err != nil {
251: 		return fmt.Errorf("attach pid failed: %w", err)
252: 	}
```
* **Explanation:** Writes the child PID to `cgroup.procs`. From this instant, the child and all future child processes it spawns belong to this cgroup.

```go
255: 	pidsFile := filepath.Join(cgroupPath, "pids.max")
256: 	if err := os.WriteFile(pidsFile, []byte(maxProcesses), 0644); err == nil {
257: 		log.Printf("cgroup: process limit set to %s (fork bomb protection)", maxProcesses)
258: 	}
259: 
260: 	memFile := filepath.Join(cgroupPath, "memory.max")
261: 	if err := os.WriteFile(memFile, []byte(maxMemoryBytes), 0644); err == nil {
262: 		log.Printf("cgroup: memory limit set to 100MB")
263: 	}
264: 
265: 	cpuFile := filepath.Join(cgroupPath, "cpu.max")
266: 	if err := os.WriteFile(cpuFile, []byte(cpuQuota), 0644); err == nil {
267: 		log.Printf("cgroup: cpu quota set to 50%% of 1 core")
268: 	}
269: 
270: 	return nil
271: }
```
* **Explanation:** Writes the limits:
  * `pids.max` = 50
  * `memory.max` = 104857600 (100MB)
  * `cpu.max` = `50000 100000` (50% quota)

```go
275: func cleanupCgroups() {
276: 	_ = os.Remove(cgroupPath)
277: }
```
* **Explanation:** Deletes the `/sys/fs/cgroup/kapsule` folder when the container exits.

---

### Deep Dive: `findBinary()` & `printUsage()` (Lines 279–317)

```go
280: func findBinary(cmd string) string {
281: 	if filepath.IsAbs(cmd) {
282: 		return cmd
283: 	}
284: 
285: 	paths := []string{
286: 		cmd,
287: 		filepath.Join("/bin", cmd),
288: 		filepath.Join("/usr/bin", cmd),
289: 		filepath.Join("/sbin", cmd),
290: 		filepath.Join("/usr/sbin", cmd),
291: 	}
292: 
293: 	for _, p := range paths {
294: 		if _, err := os.Stat(p); err == nil {
295: 			return p
296: 		}
297: 	}
298: 	return cmd
299: }
```
* **Explanation:** If the user specifies `sh` instead of `/bin/sh`, this scans standard rootfs system directories (`/bin`, `/usr/bin`, `/sbin`, `/usr/sbin`) to locate the executable binary.

```go
301: func printUsage() {
302: 	fmt.Println(`Kapsule: A Raw Linux Container Runtime (by Irene)
303: ...`)
```
* **Explanation:** Displays clear CLI instructions and flags to the terminal.

---

## 5. Live Verification & Terminal Execution

On Windows, Kapsule executes inside WSL2 where the Linux kernel and Cgroups v2 reside.

### Test 1: Verify Process Isolation (PID 1)
```powershell
wsl /mnt/host/wsl/kapsule run /mnt/host/wsl/rootfs /bin/ps aux
```
**Output:**
```text
[kapsule] Starting container with UTS, PID, and Mount namespaces...
[kapsule] Container started (Host PID: 29, Container PID: 1)
[kapsule] cgroup: process limit set to 50 (fork bomb protection)
[kapsule] cgroup: memory limit set to 100MB
[kapsule] cgroup: cpu quota set to 50% of 1 core
PID   USER     TIME  COMMAND
    1 root      0:00 /bin/ps aux
[kapsule] Container exited cleanly.
```

### Test 2: Verify Fork Bomb Protection
```powershell
wsl /mnt/host/wsl/kapsule run /mnt/host/wsl/rootfs /bin/sh -c 'for i in $(seq 1 60); do sleep 10 & done; ps aux'
```
**Output:**
```text
[kapsule] Starting container with UTS, PID, and Mount namespaces...
[kapsule] Container started (Host PID: 53, Container PID: 1)
[kapsule] cgroup: process limit set to 50 (fork bomb protection)
[kapsule] cgroup: memory limit set to 100MB
[kapsule] cgroup: cpu quota set to 50% of 1 core
/bin/sh: can't fork: Resource temporarily unavailable
```
*Result:* Linux kernel halts process creation when reaching 50 tasks, protecting the host system from crashing.

---

## 6. Senior Interview Defense Q&A

**Q1: Why did you write this in Go instead of C?**  
*Answer:* "While C is traditional for system programming, Go’s standard library `syscall` and `os/exec` packages provide direct access to the Linux kernel API without the memory management bugs of C. It allowed me to focus directly on kernel primitives like `CLONE_NEWPID` and `pivot_root` without third-party dependencies."

**Q2: What is the 're-exec' pattern and why is it required in Go?**  
*Answer:* "Go's runtime manages a multi-threaded thread pool. Because Linux namespaces apply on a per-thread basis, calling `unshare` in a running Go process can cause goroutines to jump threads and lose namespace isolation. Re-executing `/proc/self/exe` ensures the new process starts cleanly on a dedicated single-threaded process with all clone flags applied atomically at birth."

**Q3: Why use `pivot_root` instead of `chroot`?**  
*Answer:* "`chroot` only changes the root directory path pointer for path lookups. An attacker with root can call `fchdir()` on an unconfined directory file descriptor and escape. `pivot_root` swaps the root filesystem in the kernel's virtual file system (VFS) mount table. Combined with `MNT_DETACH`, the host filesystem is completely removed from the container's mount table, making escape impossible."

**Q4: How does this relate to production issues in Kubernetes?**  
*Answer:* "Kubernetes pod resource limits (`resources.limits.memory` and `resources.limits.cpu`) are implemented directly by Cgroups v2 `memory.max` and `cpu.max`. When a Kubernetes pod crashes with `OOMKilled` (Exit Code 137), that is the exact kernel mechanism we implemented here sending `SIGKILL` when memory crosses the threshold."
