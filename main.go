//go:build linux
// +build linux

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// Default limits for the container (Cgroups v2)
const (
	cgroupPath     = "/sys/fs/cgroup/kapsule"
	maxProcesses   = "50"           // pids.max: limits container to 50 processes to stop fork bombs
	maxMemoryBytes = "104857600"    // memory.max: hard cap at 100MB RAM
	cpuQuota       = "50000 100000" // cpu.max: 50ms per 100ms window = 50% of one CPU core
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("[kapsule] ")

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		// Main user entry: kapsule run <rootfs> <command> [args...]
		if len(os.Args) < 4 {
			log.Println("error: missing arguments")
			printUsage()
			os.Exit(1)
		}
		runParent(os.Args[2], os.Args[3], os.Args[3:])

	case "child":
		// Internal helper: called by runParent via /proc/self/exe to start inside new namespaces
		if len(os.Args) < 4 {
			log.Fatal("fatal: invalid child call")
		}
		runChild(os.Args[2], os.Args[3], os.Args[3:])

	case "help", "--help", "-h":
		printUsage()

	default:
		log.Printf("unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

// runParent runs on the host as the supervisor.
// It creates the namespaces, starts the child process, applies cgroup limits,
// and waits for the container to exit.
func runParent(rootfs string, cmd string, cmdArgs []string) {
	// Root is required for namespaces (CLONE_NEWNS) and cgroup configuration
	if os.Geteuid() != 0 {
		log.Fatal("kapsule needs root privileges (run with sudo)")
	}

	absRootfs, err := filepath.Abs(rootfs)
	if err != nil {
		log.Fatalf("invalid rootfs path %q: %v", rootfs, err)
	}
	if info, err := os.Stat(absRootfs); err != nil || !info.IsDir() {
		log.Fatalf("rootfs directory %q not found", absRootfs)
	}

	// Synchronization pipe:
	// The child will pause on this pipe until the parent finishes setting up cgroups.
	// This prevents a race condition where a fork bomb starts before limits are written.
	syncReader, syncWriter, err := os.Pipe()
	if err != nil {
		log.Fatalf("failed to create sync pipe: %v", err)
	}
	defer syncWriter.Close()

	// Re-exec pattern:
	// Why re-exec /proc/self/exe?
	// In Go, the runtime spawns multiple OS threads behind the scenes.
	// Linux namespaces apply per-thread, so modifying them in a running Go process can desync threads.
	// Re-running ourselves through /proc/self/exe gives us a clean, single-threaded startup inside the new namespaces.
	childArgs := append([]string{"child", absRootfs}, cmdArgs...)
	cmdExec := exec.Command("/proc/self/exe", childArgs...)

	cmdExec.Stdin = os.Stdin
	cmdExec.Stdout = os.Stdout
	cmdExec.Stderr = os.Stderr

	// Pass the read-end of our sync pipe to the child (it arrives as file descriptor 3)
	cmdExec.ExtraFiles = []*os.File{syncReader}

	// Linux Namespaces:
	// - CLONE_NEWUTS: gives container its own hostname (doesn't touch host hostname)
	// - CLONE_NEWPID: isolates process IDs (child becomes PID 1 inside the container)
	// - CLONE_NEWNS:  isolates mount points (mounts inside won't affect host)
	cmdExec.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
	}

	log.Println("Starting container with UTS, PID, and Mount namespaces...")
	if err := cmdExec.Start(); err != nil {
		syncReader.Close()
		log.Fatalf("failed to start container: %v", err)
	}

	syncReader.Close()

	// Apply Cgroups v2 resource limits to the child's host PID
	childPid := cmdExec.Process.Pid
	log.Printf("Container started (Host PID: %d, Container PID: 1)", childPid)

	if err := setupCgroups(childPid); err != nil {
		log.Printf("cgroup warning: %v", err)
	}

	// Signal the child to unblock and run
	if _, err := syncWriter.Write([]byte{1}); err != nil {
		log.Fatalf("failed to signal child process: %v", err)
	}
	syncWriter.Close()

	defer cleanupCgroups()

	// Wait for container to exit and pass back the exit code
	if err := cmdExec.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		log.Fatalf("container error: %v", err)
	}

	log.Println("Container exited cleanly.")
}

// runChild runs inside the newly isolated namespaces.
func runChild(rootfs string, cmd string, cmdArgs []string) {
	// Wait for parent to finish setting up cgroup limits before proceeding
	syncPipe := os.NewFile(uintptr(3), "sync-pipe")
	if syncPipe != nil {
		barrier := make([]byte, 1)
		_, _ = syncPipe.Read(barrier)
		syncPipe.Close()
	}

	// 1. Hostname isolation (CLONE_NEWUTS)
	if err := syscall.Sethostname([]byte("kapsule")); err != nil {
		log.Fatalf("sethostname failed: %v", err)
	}

	// 2. Filesystem isolation (pivot_root)
	if err := isolateFilesystem(rootfs); err != nil {
		log.Fatalf("pivot_root failed: %v", err)
	}

	// 3. Mount /proc for this PID namespace
	// Mounting fresh procfs lets tools like 'ps' and 'top' only see processes inside our container
	if err := os.MkdirAll("/proc", 0755); err != nil && !os.IsExist(err) {
		log.Fatalf("mkdir /proc failed: %v", err)
	}
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		log.Fatalf("mount /proc failed: %v", err)
	}

	// 4. Clean environment variables
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TERM=xterm-256color",
		"HOME=/root",
		"USER=root",
		"CONTAINER=kapsule",
	}

	target := findBinary(cmd)

	// 5. Replace current process with target binary via execve(2)
	// The binary becomes the real PID 1, and the Go runtime drops out of memory
	if err := syscall.Exec(target, cmdArgs, env); err != nil {
		log.Fatalf("execve(%q) failed: %v", target, err)
	}
}

// isolateFilesystem swaps the root filesystem to newRoot using pivot_root(2).
//
// Why pivot_root instead of chroot?
// - chroot only changes the root path pointer. A process with root can break out using
//   fchdir() on an open directory outside the jail.
// - pivot_root moves the actual mount point, and unmounting the old root with MNT_DETACH
//   completely cuts off the host filesystem from the container's mount table.
func isolateFilesystem(newRoot string) error {
	// Make root mounts private so mount changes don't leak back to the host system
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mount private failed: %w", err)
	}

	// Linux requires newRoot to be an active mount point before pivot_root can work
	if err := syscall.Mount(newRoot, newRoot, "bind", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind-mount rootfs failed: %w", err)
	}

	// pivot_root needs a folder to temporarily park the old host root
	putOld := filepath.Join(newRoot, ".oldroot")
	if err := os.MkdirAll(putOld, 0700); err != nil {
		return fmt.Errorf("create .oldroot failed: %w", err)
	}

	// Swap roots: newRoot becomes "/", old host root goes into ".oldroot"
	if err := syscall.PivotRoot(newRoot, putOld); err != nil {
		return fmt.Errorf("syscall pivot_root failed: %w", err)
	}

	// Change working directory to the new root
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir / failed: %w", err)
	}

	// MNT_DETACH (lazy unmount) immediately disconnects the old host root from the mount tree
	if err := syscall.Unmount("/.oldroot", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root failed: %w", err)
	}

	// Delete the temporary mount folder
	_ = os.Remove("/.oldroot")

	return nil
}

// setupCgroups configures Cgroups v2 for resource limits.
// - Namespaces handle isolation (what the container can SEE)
// - Cgroups handle limits (what the container can USE)
func setupCgroups(pid int) error {
	if _, err := os.Stat("/sys/fs/cgroup"); os.IsNotExist(err) {
		return fmt.Errorf("cgroups v2 not available")
	}

	// Enable controllers in root cgroup if not already enabled
	_ = os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+pids +memory +cpu"), 0644)

	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		return fmt.Errorf("mkdir cgroup failed: %w", err)
	}

	// Attach child process to our cgroup
	procsFile := filepath.Join(cgroupPath, "cgroup.procs")
	if err := os.WriteFile(procsFile, []byte(strconv.Itoa(pid)), 0644); err != nil {
		return fmt.Errorf("attach pid failed: %w", err)
	}

	// pids.max: prevents fork bombs by capping total processes inside container
	pidsFile := filepath.Join(cgroupPath, "pids.max")
	if err := os.WriteFile(pidsFile, []byte(maxProcesses), 0644); err == nil {
		log.Printf("cgroup: process limit set to %s (fork bomb protection)", maxProcesses)
	}

	// memory.max: hard cap on RAM usage to prevent host memory exhaustion
	memFile := filepath.Join(cgroupPath, "memory.max")
	if err := os.WriteFile(memFile, []byte(maxMemoryBytes), 0644); err == nil {
		log.Printf("cgroup: memory limit set to 100MB")
	}

	// cpu.max: limits CPU quota (50ms per 100ms = 50%% of one CPU core)
	cpuFile := filepath.Join(cgroupPath, "cpu.max")
	if err := os.WriteFile(cpuFile, []byte(cpuQuota), 0644); err == nil {
		log.Printf("cgroup: cpu quota set to 50%% of 1 core")
	}

	return nil
}

func cleanupCgroups() {
	_ = os.Remove(cgroupPath)
}

// findBinary checks standard bin directories if a full path isn't provided
func findBinary(cmd string) string {
	if filepath.IsAbs(cmd) {
		return cmd
	}

	paths := []string{
		cmd,
		filepath.Join("/bin", cmd),
		filepath.Join("/usr/bin", cmd),
		filepath.Join("/sbin", cmd),
		filepath.Join("/usr/sbin", cmd),
	}

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return cmd
}

func printUsage() {
	fmt.Println(`Kapsule: A Raw Linux Container Runtime

Usage:
  sudo ./kapsule run <rootfs-path> <command> [args...]

Examples:
  sudo ./kapsule run ./rootfs /bin/sh
  sudo ./kapsule run ./rootfs /bin/ps aux
  sudo ./kapsule run ./rootfs /bin/hostname

Under the hood:
  - Namespaces: UTS (hostname), PID (processes), Mount (filesystems)
  - pivot_root: True root filesystem swap, immune to chroot jailbreaks
  - Cgroups v2: Limits processes (pids.max=50), RAM (100MB), and CPU (50%)
  - Re-exec: Uses /proc/self/exe to guarantee clean single-threaded PID 1`)
}
