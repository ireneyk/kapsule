// ==============================================================================
// Project: Kapsule - A Raw Linux Container Runtime
// Author:  Irene
//
// Description:
// A from-scratch, dependency-free Linux container runtime written in pure Go.
// Kapsule demonstrates the foundational kernel-level primitives that power
// industry-standard container engines like Docker, Podman, CRI-O, and runc.
//
// Core Mechanisms Implemented:
// 1. Linux Namespaces (UTS, PID, Mount/NS) via clone(2) for kernel resource isolation.
// 2. The Re-Exec Pattern (/proc/self/exe) to overcome Go runtime M:N threading
//    and guarantee deterministic PID 1 process execution.
// 3. True Root Filesystem Pivoting via pivot_root(2) with lazy unmounts (MNT_DETACH)
//    to permanently sever host filesystem access (preventing chroot escapes).
// 4. Linux Control Groups v2 (cgroupv2) for resource governance, enforcing
//    pids.max (fork bomb immunity), memory.max, and cpu.max.
// 5. Fresh virtual filesystem mount (/proc) bound strictly to the isolated PID namespace.
// 6. Parent-Child Synchronization Pipe to eliminate cgroup initialization race conditions.
// ==============================================================================

//go:build linux
// +build linux

package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// Cgroup v2 configuration parameters
const (
	cgroupRoot     = "/sys/fs/cgroup"
	kapsuleCgroup  = "/sys/fs/cgroup/kapsule"
	defaultMaxPids = "50"        // Limits container to 50 processes (prevents fork bombs)
	defaultMaxMem  = "104857600" // 100 Megabytes (100 * 1024 * 1024 bytes)
	defaultCPUQuota = "50000 100000" // 50ms per 100ms period (50% of one CPU core)
)

func main() {
	// Configure minimal logger for clean, readable output
	log.SetFlags(0)
	log.SetPrefix("[Kapsule] ")

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		// User command entrypoint: kapsule run <rootfs> <command> [args...]
		if len(os.Args) < 4 {
			log.Println("Error: Missing required arguments for 'run'")
			printUsage()
			os.Exit(1)
		}
		rootfsPath := os.Args[2]
		command := os.Args[3]
		commandArgs := os.Args[3:]
		runParent(rootfsPath, command, commandArgs)

	case "child":
		// Internal entrypoint invoked via /proc/self/exe during the re-exec pattern.
		// At this point, the process is executing inside the unshared namespaces.
		if len(os.Args) < 4 {
			log.Fatalf("Fatal: Invalid internal child invocation")
		}
		rootfsPath := os.Args[2]
		command := os.Args[3]
		commandArgs := os.Args[3:]
		runChild(rootfsPath, command, commandArgs)

	case "help", "--help", "-h":
		printUsage()

	default:
		log.Printf("Unknown command: %q\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

// ==============================================================================
// 1. THE PARENT PROCESS & THE RE-EXEC PATTERN
// ==============================================================================

// runParent acts as the container supervisor on the host system.
// It creates the synchronization pipe, provisions Linux namespaces via Cloneflags,
// enforces Cgroups v2 resource limits on the child PID, and waits for container exit.
func runParent(rootfs string, cmd string, cmdArgs []string) {
	// --------------------------------------------------------------------------
	// Pre-flight Check: Privilege Verification
	// Creating namespaces (CLONE_NEWNS) and manipulating cgroups requires CAP_SYS_ADMIN.
	// --------------------------------------------------------------------------
	if os.Geteuid() != 0 {
		log.Fatalf("Kapsule requires root privileges (CAP_SYS_ADMIN). Please run with 'sudo'.")
	}

	// Resolve the absolute path to rootfs so the child can locate it regardless of directory changes.
	absRootfs, err := filepath.Abs(rootfs)
	if err != nil {
		log.Fatalf("Failed to resolve absolute path for rootfs %q: %v", rootfs, err)
	}

	// Verify rootfs directory exists and is valid
	info, err := os.Stat(absRootfs)
	if err != nil || !info.IsDir() {
		log.Fatalf("Provided rootfs path %q does not exist or is not a directory", absRootfs)
	}

	// --------------------------------------------------------------------------
	// The Re-Exec Synchronization Pipe
	// Race Condition Problem:
	// If the child immediately starts executing user code, a malicious process could
	// trigger a fork bomb BEFORE the parent has finished writing 'pids.max' to the cgroup!
	//
	// Solution:
	// We create an anonymous OS pipe. The child blocks waiting to read from the pipe.
	// The parent configures Cgroups v2 on the child's host PID, and then writes 1 byte
	// to unblock the child. This is the exact synchronization model used by runc.
	// --------------------------------------------------------------------------
	syncReader, syncWriter, err := os.Pipe()
	if err != nil {
		log.Fatalf("Failed to create synchronization pipe: %v", err)
	}
	defer syncWriter.Close()

	// --------------------------------------------------------------------------
	// The "Re-exec" Pattern
	// Why re-exec /proc/self/exe?
	// 1. In Linux, calling clone(CLONE_NEWPID) places *children* of the calling process
	//    into a new PID namespace. The caller itself remains in the parent namespace.
	//    Therefore, to have our container initialization logic run as PID 1, we must
	//    spawn a child process.
	// 2. The Go runtime is inherently multi-threaded (operating system threads 'M'
	//    managed by the Go scheduler). Modifying namespaces in a multi-threaded Go process
	//    via raw unshare(2) or setns(2) causes desynchronization across OS threads.
	// 3. Re-executing /proc/self/exe guarantees a pristine single-threaded initialization
	//    sequence directly inside the target namespaces.
	// --------------------------------------------------------------------------
	childArgs := append([]string{"child", absRootfs}, cmdArgs...)
	cmdExec := exec.Command("/proc/self/exe", childArgs...)

	// Pass host standard I/O streams directly into the container process
	cmdExec.Stdin = os.Stdin
	cmdExec.Stdout = os.Stdout
	cmdExec.Stderr = os.Stderr

	// ExtraFiles passes file descriptors to the child starting at FD 3.
	// syncReader will be accessible in child at file descriptor 3.
	cmdExec.ExtraFiles = []*os.File{syncReader}

	// --------------------------------------------------------------------------
	// Namespace Allocation via SysProcAttr.Cloneflags
	//
	// CLONE_NEWUTS: Isolates Hostname and NIS domain name (UTS = UNIX Timesharing System).
	//               Allows the container to have its own hostname without altering the host.
	//
	// CLONE_NEWPID: Virtualizes Process IDs. The newly spawned child process becomes PID 1
	//               inside the container. It cannot see or signal any processes on the host.
	//
	// CLONE_NEWNS:  Isolates the Virtual Filesystem (VFS) mount table (NS = Namespace).
	//               Mounts and unmounts performed inside this container remain completely
	//               invisible to the host system.
	// --------------------------------------------------------------------------
	cmdExec.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWPID |
			syscall.CLONE_NEWNS,
	}

	log.Printf("Spawning container process [Namespaces: UTS, PID, Mount]...")
	if err := cmdExec.Start(); err != nil {
		syncReader.Close()
		log.Fatalf("Failed to start container child process: %v", err)
	}

	// Close parent's copy of the read end of the pipe
	syncReader.Close()

	// --------------------------------------------------------------------------
	// Cgroups v2 Enforcement
	// Attach the child process (using its host PID) to our Cgroups v2 slice
	// and write resource bounds (pids.max, memory.max, cpu.max).
	// --------------------------------------------------------------------------
	childPid := cmdExec.Process.Pid
	log.Printf("Container child launched with Host PID: %d (Virtual PID: 1)", childPid)

	if err := setupCgroups(childPid); err != nil {
		log.Printf("Warning: Failed to fully configure Cgroups v2: %v", err)
	}

	// --------------------------------------------------------------------------
	// Signal Child to Proceed
	// Write 1 synchronization byte to release the child from its barrier.
	// --------------------------------------------------------------------------
	if _, err := syncWriter.Write([]byte{1}); err != nil {
		log.Fatalf("Failed to signal child process via sync pipe: %v", err)
	}
	syncWriter.Close()

	// Ensure Cgroup cleanup on container termination
	defer cleanupCgroups()

	// Wait for the containerized process to exit
	if err := cmdExec.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			// Forward child exit code to caller
			os.Exit(exitErr.ExitCode())
		}
		log.Fatalf("Container exited with error: %v", err)
	}

	log.Println("Container process terminated cleanly. Cgroups cleaned up.")
}

// ==============================================================================
// 2. THE CHILD PROCESS & CONTAINER INITIALIZATION
// ==============================================================================

// runChild executes inside the newly created namespaces.
// It synchronizes with the parent, isolates the hostname, securely pivots the root
// filesystem via pivot_root(2), mounts /proc, and executes the target binary as PID 1.
func runChild(rootfs string, cmd string, cmdArgs []string) {
	// --------------------------------------------------------------------------
	// Synchronization Barrier: Wait for Parent Cgroup Setup
	// Read 1 byte from FD 3 (passed via cmd.ExtraFiles).
	// --------------------------------------------------------------------------
	syncPipe := os.NewFile(uintptr(3), "sync-pipe")
	if syncPipe != nil {
		barrierBuf := make([]byte, 1)
		_, _ = syncPipe.Read(barrierBuf)
		syncPipe.Close()
	}

	// --------------------------------------------------------------------------
	// 1. UTS Namespace Isolation: Hostname Configuration
	// Under CLONE_NEWUTS, sethostname(2) changes the hostname solely for this
	// container's UTS namespace. The host's hostname remains untouched.
	// --------------------------------------------------------------------------
	containerHostname := "kapsule"
	if err := syscall.Sethostname([]byte(containerHostname)); err != nil {
		log.Fatalf("[Child] sethostname(%q) failed: %v", containerHostname, err)
	}

	// --------------------------------------------------------------------------
	// 2. Secure Filesystem Isolation: pivot_root(2)
	// Swap the root filesystem securely to rootfs and detach the host mount tree.
	// --------------------------------------------------------------------------
	if err := securePivotRoot(rootfs); err != nil {
		log.Fatalf("[Child] Filesystem isolation via pivot_root failed: %v", err)
	}

	// --------------------------------------------------------------------------
	// 3. Procfs Mount for Isolated PID Namespace
	// The /proc virtual filesystem provides a window into kernel process state.
	// Because we are in a new PID namespace (CLONE_NEWPID), mounting a new instance
	// of procfs at /proc exposes ONLY the processes within this container!
	// Tools like 'ps', 'top', and 'kill' will see this child as PID 1.
	// --------------------------------------------------------------------------
	if err := os.MkdirAll("/proc", 0755); err != nil && !os.IsExist(err) {
		log.Fatalf("[Child] Failed to ensure /proc mount directory: %v", err)
	}

	// Mount procfs: source="proc", target="/proc", fstype="proc"
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		log.Fatalf("[Child] Failed to mount /proc virtual filesystem: %v", err)
	}

	// --------------------------------------------------------------------------
	// 4. Clean Environment & Binary Resolution
	// Replace host environment variables with a clean container environment.
	// --------------------------------------------------------------------------
	containerEnv := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TERM=xterm-256color",
		"HOME=/root",
		"USER=root",
		"CONTAINER=kapsule",
	}

	// Resolve the target binary path within the new root filesystem
	targetBinary := resolveBinary(cmd)

	// --------------------------------------------------------------------------
	// 5. execve(2) - Replacing Process Image
	// syscall.Exec invokes the execve(2) system call.
	// It replaces the current Go process image completely with the target binary.
	// Benefits:
	// - The target command runs as TRUE PID 1 inside the container.
	// - Zero Go runtime memory or supervisor overhead remains in memory.
	// - Signal handling (SIGTERM, SIGINT) is passed directly to the containerized binary.
	// --------------------------------------------------------------------------
	if err := syscall.Exec(targetBinary, cmdArgs, containerEnv); err != nil {
		log.Fatalf("[Child] execve(%q) failed: %v", targetBinary, err)
	}
}

// ==============================================================================
// 3. PIVOT_ROOT VS CHROOT: SECURE ROOT SWAPPING
// ==============================================================================

// securePivotRoot implements secure root filesystem isolation using pivot_root(2).
//
// WHY PIVOT_ROOT INSTEAD OF CHROOT?
// ---------------------------------
// 1. The chroot(2) Vulnerability:
//    - chroot only changes the current process's view of "/" (current->fs->root pointer).
//    - It DOES NOT change the mount table or detach the host mount namespace.
//    - Any process with root privileges (CAP_SYS_CHROOT) can trivially escape a chroot jail
//      by opening a file descriptor to a directory outside the jail before chrooting,
//      or using fchdir(2) on an open parent descriptor:
//        int fd = open(".", O_RDONLY);
//        chroot("empty_dir");
//        fchdir(fd);
//        chroot("../../../../../"); // ESCAPED!
//
// 2. The pivot_root(2) Security Guarantee:
//    - pivot_root operates on the Virtual Filesystem (VFS) mount table itself.
//    - It moves the old root mount point to a designated subdirectory (put_old)
//      and binds the new rootfs directory as the new root mount point of the namespace.
//    - After pivoting, we invoke umount2(put_old, MNT_DETACH) (lazy unmount).
//    - The host filesystem is completely detached and eradicated from the mount table!
//    - Even a root process (UID 0) inside the container cannot traverse or escape to
//      the host filesystem, because the host mount simply does not exist in the namespace.
//    - This is why the Open Container Initiative (OCI) specification and runc mandate
//      pivot_root for all production containers.
func securePivotRoot(newRoot string) error {
	// Step 1: Make all mounts in this namespace PRIVATE.
	// In modern systemd-based Linux systems, mounts are shared (MS_SHARED) by default.
	// If mounts are shared, any mount/unmount operation performed inside our container
	// will propagate back to the host system and corrupt host mounts!
	// Setting MS_REC|MS_PRIVATE isolates this mount namespace entirely.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("failed to make mount tree private (MS_PRIVATE): %w", err)
	}

	// Step 2: Bind-mount newRoot to itself.
	// Linux kernel requirement for pivot_root(2):
	// 'new_root' must be an active mount point. A regular host directory is not a mount point.
	// Bind-mounting newRoot onto itself satisfies this kernel invariant.
	if err := syscall.Mount(newRoot, newRoot, "bind", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("failed to bind-mount rootfs to itself: %w", err)
	}

	// Step 3: Create temporary directory for put_old.
	// Linux kernel requirement for pivot_root(2):
	// 'put_old' must be at or underneath 'new_root'.
	putOld := filepath.Join(newRoot, ".pivot_old")
	if err := os.MkdirAll(putOld, 0700); err != nil {
		return fmt.Errorf("failed to create put_old directory %s: %w", putOld, err)
	}

	// Step 4: Execute pivot_root(2).
	// Swaps newRoot to become "/" and places the host root mount at "/.pivot_old".
	if err := syscall.PivotRoot(newRoot, putOld); err != nil {
		return fmt.Errorf("pivot_root(2) failed: %w", err)
	}

	// Step 5: Change current working directory to the new root "/".
	// Ensures that relative path resolutions start at the new rootfs.
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir('/') failed: %w", err)
	}

	// Step 6: Detach and unmount the old host root.
	// MNT_DETACH performs a lazy unmount: it immediately disconnects /.pivot_old
	// from the mount table, permanently cutting all access to host files.
	relativePutOld := "/.pivot_old"
	if err := syscall.Unmount(relativePutOld, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("failed to lazy-unmount old root %s: %w", relativePutOld, err)
	}

	// Step 7: Clean up the temporary mount point directory.
	if err := os.Remove(relativePutOld); err != nil {
		// Non-fatal if directory is busy, but good housekeeping
		return fmt.Errorf("failed to remove %s: %w", relativePutOld, err)
	}

	return nil
}

// ==============================================================================
// 4. CGROUPS (V2): RESOURCE GOVERNANCE & FORK BOMB MITIGATION
// ==============================================================================

// setupCgroups configures Linux Control Groups v2 (cgroupv2) for resource governance.
//
// NAMESPACES VS CGROUPS:
// - Namespaces govern ISOLATION: What a process can SEE (its own PID list, hostname, VFS tree).
// - Cgroups govern ACCOUNTING & LIMITS: What a process can USE (CPU cycles, RAM bytes, Thread counts).
//
// In Cgroups v2 (Unified Hierarchy):
// - Located at /sys/fs/cgroup.
// - All controllers (cpu, memory, pids) exist in a single unified tree.
// - We create a child cgroup simply by creating a directory: /sys/fs/cgroup/kapsule.
// - Attaching a process is done by writing its host PID to cgroup.procs.
func setupCgroups(pid int) error {
	// Verify Cgroups v2 is available on the host
	if _, err := os.Stat(cgroupRoot); os.IsNotExist(err) {
		return fmt.Errorf("cgroup filesystem not found at %s", cgroupRoot)
	}

	// 1. Create our container's dedicated cgroup slice
	if err := os.MkdirAll(kapsuleCgroup, 0755); err != nil {
		return fmt.Errorf("failed to create cgroup directory %s: %w", kapsuleCgroup, err)
	}

	// 2. Attach Process to Cgroup
	// Writing the host PID to 'cgroup.procs' places the container process and all of its
	// future child processes/threads into this control group.
	procsPath := filepath.Join(kapsuleCgroup, "cgroup.procs")
	if err := os.WriteFile(procsPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		return fmt.Errorf("failed to write PID %d to %s: %w", pid, procsPath, err)
	}

	// 3. Fork Bomb Mitigation via 'pids.max'
	// What happens during a fork bomb? (e.g., :(){ :|:& };: in bash):
	// A runaway process recursively spawns children, exhausting the kernel's global
	// PID table (/proc/sys/kernel/pid_max). Once exhausted, the entire host freezes
	// because no process (including sshd or admin shells) can fork.
	//
	// Cgroup v2 Solution:
	// By setting 'pids.max' to a low threshold (e.g., 50), the kernel counts active
	// tasks within this specific cgroup slice. When the 51st fork(2) is attempted,
	// the kernel immediately returns EAGAIN ("Resource temporarily unavailable").
	// The host system remains completely unaffected and responsive!
	pidsMaxPath := filepath.Join(kapsuleCgroup, "pids.max")
	if err := os.WriteFile(pidsMaxPath, []byte(defaultMaxPids), 0644); err != nil {
		log.Printf("[Cgroups] Note: Unable to set pids.max (%v). Check /sys/fs/cgroup/cgroup.subtree_control", err)
	} else {
		log.Printf("[Cgroups] Configured pids.max = %s (Fork bomb mitigation enabled)", defaultMaxPids)
	}

	// 4. Memory Limit via 'memory.max'
	// Sets a hard memory limit of 100MB. If processes in this container exceed 100MB,
	// the kernel Out-Of-Memory (OOM) killer terminates tasks within this cgroup,
	// safeguarding the host from memory starvation.
	memMaxPath := filepath.Join(kapsuleCgroup, "memory.max")
	if err := os.WriteFile(memMaxPath, []byte(defaultMaxMem), 0644); err != nil {
		log.Printf("[Cgroups] Note: Unable to set memory.max (%v). Check /sys/fs/cgroup/cgroup.subtree_control", err)
	} else {
		log.Printf("[Cgroups] Configured memory.max = %s bytes (100MB RAM cap)", defaultMaxMem)
	}

	// 5. CPU Bandwidth Quota via 'cpu.max'
	// Configures CFS (Completely Fair Scheduler) quota.
	// "50000 100000" restricts the container to 50,000 microseconds of CPU time
	// per 100,000 microsecond period (strictly 50% of one core).
	cpuMaxPath := filepath.Join(kapsuleCgroup, "cpu.max")
	if err := os.WriteFile(cpuMaxPath, []byte(defaultCPUQuota), 0644); err != nil {
		log.Printf("[Cgroups] Note: Unable to set cpu.max (%v). Check /sys/fs/cgroup/cgroup.subtree_control", err)
	} else {
		log.Printf("[Cgroups] Configured cpu.max = %s (50%% single-core bandwidth limit)", defaultCPUQuota)
	}

	return nil
}

// cleanupCgroups removes the cgroup slice directory after all processes terminate.
// In Cgroups v2, a cgroup directory can only be deleted once its 'cgroup.procs' is empty.
func cleanupCgroups() {
	if err := os.Remove(kapsuleCgroup); err != nil {
		// If child processes are still terminating, best-effort removal
		_ = err
	}
}

// ==============================================================================
// 5. UTILITY & HELPER FUNCTIONS
// ==============================================================================

// resolveBinary searches for the target binary inside the container rootfs.
func resolveBinary(cmd string) string {
	// If path is already absolute, verify and return
	if filepath.IsAbs(cmd) {
		return cmd
	}

	// Search common Linux binary locations inside the container rootfs
	searchPaths := []string{
		cmd,
		filepath.Join("/bin", cmd),
		filepath.Join("/usr/bin", cmd),
		filepath.Join("/sbin", cmd),
		filepath.Join("/usr/sbin", cmd),
	}

	for _, p := range searchPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	// Default fallback to user input
	return cmd
}

// printUsage displays CLI help and architecture overview.
func printUsage() {
	usage := `
Kapsule: A Raw Linux Container Runtime
Original Author: Irene

A pure Go container runtime implementing kernel isolation without external dependencies.

Usage:
  sudo kapsule run <rootfs-path> <command> [args...]
  kapsule help

Commands:
  run     Initialize isolated namespaces, apply Cgroups v2, pivot root, and execute command.
  help    Print this documentation and architecture guide.

Examples:
  sudo ./kapsule run ./rootfs /bin/sh
  sudo ./kapsule run ./rootfs /bin/ps aux
  sudo ./kapsule run ./rootfs /bin/hostname

Key Architectural Primitives:
  1. Namespaces:
     - CLONE_NEWUTS: Dedicated hostname ('kapsule')
     - CLONE_NEWPID: Dedicated PID table (Child becomes PID 1)
     - CLONE_NEWNS:  Dedicated Virtual Filesystem (VFS) mount table
  2. The Re-Exec Pattern:
     - Re-executes /proc/self/exe to guarantee clean single-threaded PID 1 startup
  3. Secure pivot_root(2):
     - Bind-mounts rootfs, executes pivot_root, and lazy-unmounts (MNT_DETACH) old root
  4. Cgroups v2 Resource Governance:
     - pids.max   = 50 (Fork bomb protection)
     - memory.max = 100MB (OOM defense)
     - cpu.max    = 50% CPU bandwidth
`
	fmt.Println(usage)
}
