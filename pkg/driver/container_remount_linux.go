package driver

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"golang.org/x/sys/unix"
)

// remountInContainers fixes stale FUSE mounts inside pod containers after
// the health monitor has recovered a volume on the host.
//
// When the CSI driver recovers a dead FUSE mount (unmount stale staging,
// re-stage, re-bind publish paths), the host-level mounts are correct.
// However, container runtimes create the pod's volume bind-mount with
// rprivate propagation by default, so the host-side remount does NOT
// propagate into existing containers. The pod still sees the old dead
// FUSE mount ("Transport endpoint is not connected").
//
// This function enters each affected container's mount namespace via
// setns(2) and creates a fresh bind mount from the recovered staging
// path, replacing the stale one. This requires the CSI driver pod to
// run with hostPID: true so it can see container processes in /proc.
//
// oldDevice is the "major:minor" string of the dead FUSE mount that was
// at stagingPath before recovery. It is used to identify the
// corresponding stale mount entry inside each container's mountinfo.
func remountInContainers(publishPath, stagingPath, oldDevice string) {
	podUID := extractPodUID(publishPath)
	if podUID == "" {
		glog.V(4).Infof("container remount: could not extract pod UID from %s", publishPath)
		return
	}

	pids, err := findContainerPIDsForPod(podUID)
	if err != nil {
		glog.V(4).Infof("container remount: could not find container PIDs for pod %s: %v (hostPID may not be enabled)", podUID, err)
		return
	}
	if len(pids) == 0 {
		glog.V(4).Infof("container remount: no container PIDs found for pod %s", podUID)
		return
	}

	for _, pid := range pids {
		containerMountPath, err := findContainerMountByDevice(pid, oldDevice)
		if err != nil || containerMountPath == "" {
			continue
		}

		glog.Infof("container remount: fixing stale mount %s in container PID %d (pod %s)", containerMountPath, pid, podUID)
		if err := remountViaSetns(pid, containerMountPath, stagingPath); err != nil {
			glog.Warningf("container remount: failed to remount %s in PID %d: %v", containerMountPath, pid, err)
		} else {
			glog.Infof("container remount: successfully remounted %s in PID %d", containerMountPath, pid)
		}
	}
}

// remountStaleFuseInContainers is a scan-based variant used by
// retryPublishPaths where the old device is unknown. It enters each
// affected container, finds FUSE mounts that are stale (ENOTCONN), and
// replaces them with a fresh bind from stagingPath.
func remountStaleFuseInContainers(publishPath, stagingPath string) {
	podUID := extractPodUID(publishPath)
	if podUID == "" {
		return
	}

	pids, err := findContainerPIDsForPod(podUID)
	if err != nil || len(pids) == 0 {
		return
	}

	for _, pid := range pids {
		entries, err := parseMountInfo(pid)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !isFuseFS(e.fstype) {
				continue
			}
			// Check accessibility via /proc/<pid>/root/<mountpoint>.
			checkPath := fmt.Sprintf("/proc/%d/root%s", pid, e.mountpoint)
			_, statErr := os.Stat(checkPath)
			if !isStaleMount(statErr) {
				continue
			}

			glog.Infof("container remount: fixing stale mount %s in container PID %d (pod %s)", e.mountpoint, pid, podUID)
			if err := remountViaSetns(pid, e.mountpoint, stagingPath); err != nil {
				glog.Warningf("container remount: failed to remount %s in PID %d: %v", e.mountpoint, pid, err)
			} else {
				glog.Infof("container remount: successfully remounted %s in PID %d", e.mountpoint, pid)
			}
		}
	}
}

// extractPodUID extracts the pod UID from a CSI publish path.
// Expected format: .../pods/<uid>/volumes/...
func extractPodUID(publishPath string) string {
	parts := strings.Split(publishPath, string(os.PathSeparator))
	for i, part := range parts {
		if part == "pods" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// findContainerPIDsForPod returns one PID per unique mount namespace
// for containers belonging to the given pod. It scans /proc/*/cgroup
// for the pod UID. Requires hostPID: true on the CSI driver pod.
func findContainerPIDsForPod(podUID string) ([]int, error) {
	// Kubernetes uses both dash-separated and underscore-separated UIDs
	// in cgroup paths depending on the container runtime.
	normalizedUID := strings.ReplaceAll(podUID, "-", "_")

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}

	// Collect one PID per mount namespace to avoid redundant remounts.
	seenNS := make(map[string]bool)
	var pids []int

	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		cgroupData, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
		if err != nil {
			continue
		}

		content := string(cgroupData)
		if !strings.Contains(content, podUID) && !strings.Contains(content, normalizedUID) {
			continue
		}

		// Deduplicate by mount namespace inode so we remount once per
		// container, not once per process.
		nsPath := fmt.Sprintf("/proc/%d/ns/mnt", pid)
		nsLink, err := os.Readlink(nsPath)
		if err != nil {
			continue
		}

		// Skip processes that share the CSI driver's own mount namespace.
		selfNS, err := os.Readlink("/proc/self/ns/mnt")
		if err == nil && nsLink == selfNS {
			continue
		}

		if !seenNS[nsLink] {
			seenNS[nsLink] = true
			pids = append(pids, pid)
		}
	}

	return pids, nil
}

// mountInfoEntry holds a parsed line from /proc/<pid>/mountinfo.
type mountInfoEntry struct {
	device     string // "major:minor"
	mountpoint string
	fstype     string
}

// parseMountInfo parses /proc/<pid>/mountinfo and returns entries.
func parseMountInfo(pid int) ([]mountInfoEntry, error) {
	path := fmt.Sprintf("/proc/%d/mountinfo", pid)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []mountInfoEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 7 {
			continue
		}
		// Format: id parent major:minor root mountpoint opts [optional...] - fstype source superopts
		device := fields[2]
		mountpoint := fields[4]

		// Find the "-" separator to get fstype
		sepIdx := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sepIdx = i
				break
			}
		}
		var fstype string
		if sepIdx >= 0 && sepIdx+1 < len(fields) {
			fstype = fields[sepIdx+1]
		}

		entries = append(entries, mountInfoEntry{
			device:     device,
			mountpoint: mountpoint,
			fstype:     fstype,
		})
	}
	return entries, scanner.Err()
}

// getMountDevice returns the "major:minor" device string for a mount
// at the given path by parsing /proc/self/mountinfo.
func getMountDevice(mountPath string) (string, error) {
	entries, err := parseMountInfo(os.Getpid())
	if err != nil {
		// If /proc/self/mountinfo is not parseable (e.g., in some
		// container setups), try PID 0 pseudo-entry.
		return "", fmt.Errorf("parse self mountinfo: %w", err)
	}

	// Find the most specific (longest) matching mountpoint, since there
	// may be nested mounts.
	var best mountInfoEntry
	for _, e := range entries {
		if e.mountpoint == mountPath && len(e.mountpoint) >= len(best.mountpoint) {
			best = e
		}
	}
	if best.device == "" {
		return "", fmt.Errorf("no mount entry found for %s", mountPath)
	}
	return best.device, nil
}

// findContainerMountByDevice finds a mount inside a container's mount
// namespace whose device matches oldDevice and whose fstype is FUSE.
// Returns the container-side mount path (e.g., "/frontendrelease").
func findContainerMountByDevice(pid int, oldDevice string) (string, error) {
	entries, err := parseMountInfo(pid)
	if err != nil {
		return "", err
	}

	for _, e := range entries {
		if e.device == oldDevice && isFuseFS(e.fstype) {
			return e.mountpoint, nil
		}
	}
	return "", nil
}

// isFuseFS returns true if the fstype indicates a FUSE filesystem mount.
// "fusectl" is excluded — it is the FUSE control filesystem, not a user mount.
func isFuseFS(fstype string) bool {
	return strings.HasPrefix(fstype, "fuse") && fstype != "fusectl"
}

// isStaleMount returns true if the error indicates a dead FUSE mount
// ("Transport endpoint is not connected" or similar).
func isStaleMount(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.ENOTCONN || errno == syscall.EIO
	}
	return false
}

// remountViaSetns enters a container's mount namespace using setns(2),
// unmounts the stale FUSE mount, and creates a fresh bind mount from
// the recovered staging path. The staging path is accessible from
// inside the container's mount namespace because it resides on the
// host filesystem under the kubelet directory tree.
func remountViaSetns(containerPID int, containerMountPath, stagingPath string) error {
	// Pin this goroutine to the current OS thread for the duration of
	// the namespace switch. No other goroutine will be scheduled on
	// this thread, preventing accidental cross-namespace operations.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Give this thread its own filesystem context (cwd, root, umask).
	// Go threads share their fs_struct by default, and the kernel
	// rejects setns(CLONE_NEWNS) when the calling thread shares its
	// fs_struct with other threads. unshare(CLONE_FS) breaks that
	// sharing so setns can proceed.
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return fmt.Errorf("unshare CLONE_FS: %w", err)
	}

	// Save our current mount namespace so we can restore it.
	origNS, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		return fmt.Errorf("open current mount ns: %w", err)
	}
	defer origNS.Close()

	// Open the container's mount namespace.
	containerNSPath := fmt.Sprintf("/proc/%d/ns/mnt", containerPID)
	containerNS, err := os.Open(containerNSPath)
	if err != nil {
		return fmt.Errorf("open container mount ns %s: %w", containerNSPath, err)
	}
	defer containerNS.Close()

	// Enter the container's mount namespace.
	if err := unix.Setns(int(containerNS.Fd()), unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("setns to container PID %d: %w", containerPID, err)
	}

	// ALWAYS restore our namespace before returning, even on error.
	defer func() {
		if restoreErr := unix.Setns(int(origNS.Fd()), unix.CLONE_NEWNS); restoreErr != nil {
			// This is a critical error -- the goroutine is stuck in the
			// wrong namespace. LockOSThread prevents it from affecting
			// other goroutines, and the deferred UnlockOSThread will
			// retire the thread. Log loudly.
			glog.Errorf("container remount: CRITICAL: failed to restore mount namespace: %v", restoreErr)
		}
	}()

	// Lazy-unmount the stale FUSE mount inside the container.
	// MNT_DETACH ensures this succeeds even if processes have open
	// files, letting them drain while new accesses use the fresh mount.
	if umountErr := unix.Unmount(containerMountPath, unix.MNT_DETACH); umountErr != nil {
		glog.V(4).Infof("container remount: umount %s in PID %d: %v (may already be unmounted)", containerMountPath, containerPID, umountErr)
	}

	// Bind-mount the staging path into the container. The staging path
	// resides on the host filesystem under /var/lib/kubelet/plugins/
	// which is accessible from inside the container's mount namespace
	// because both the CSI driver and pod containers share the same
	// host filesystem root for kubelet paths.
	if err := unix.Mount(stagingPath, containerMountPath, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind mount %s -> %s in container PID %d: %w", stagingPath, containerMountPath, containerPID, err)
	}

	return nil
}
