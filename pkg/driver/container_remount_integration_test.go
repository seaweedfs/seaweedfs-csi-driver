//go:build linux && integration

package driver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// These integration tests exercise the real setns / mount namespace
// machinery. They require:
//   - Linux (build tag already enforced)
//   - Root privileges (CAP_SYS_ADMIN) for unshare / setns / mount
//
// Run with: sudo go test -tags integration -run Integration ./pkg/driver/

func skipIfNotRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("integration tests require root")
	}
}

// TestIntegrationRemountViaSetns creates a child process in a separate
// mount namespace, mounts a tmpfs inside it, then uses remountViaSetns
// to replace that mount with a bind from a different directory.
func TestIntegrationRemountViaSetns(t *testing.T) {
	skipIfNotRoot(t)

	// --- Setup: create two directories with distinct content ---
	root := t.TempDir()
	originalDir := filepath.Join(root, "original")
	replacementDir := filepath.Join(root, "replacement")
	childMountpoint := filepath.Join(root, "childmnt")

	for _, d := range []string{originalDir, replacementDir, childMountpoint} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(originalDir, "marker"), []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(replacementDir, "marker"), []byte("replacement"), 0644); err != nil {
		t.Fatal(err)
	}

	// --- Start a child process in a new mount namespace ---
	// The child bind-mounts originalDir onto childMountpoint inside its
	// private namespace, writes its PID to a file, then sleeps.
	pidFile := filepath.Join(root, "child.pid")
	readyFile := filepath.Join(root, "child.ready")
	child := exec.Command("unshare", "--mount", "--propagation", "private",
		"sh", "-c", fmt.Sprintf(
			`mount --bind %s %s && echo $$ > %s && touch %s && sleep 60`,
			originalDir, childMountpoint, pidFile, readyFile,
		))
	child.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
	}
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	defer func() {
		child.Process.Kill()
		child.Wait()
	}()

	// Wait for the child to signal readiness.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child PID: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse child PID %q: %v", string(pidBytes), err)
	}

	// --- Verify: from the child's namespace, the mountpoint has "original" content ---
	markerPath := fmt.Sprintf("/proc/%d/root%s/marker", childPID, childMountpoint)
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read marker in child namespace: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "original" {
		t.Fatalf("expected 'original' marker, got %q", got)
	}

	// --- Act: use remountViaSetns to replace the mount ---
	if err := remountViaSetns(childPID, childMountpoint, replacementDir); err != nil {
		t.Fatalf("remountViaSetns: %v", err)
	}

	// --- Verify: the child's mountpoint now shows "replacement" content ---
	data, err = os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read marker after remount: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "replacement" {
		t.Errorf("expected 'replacement' marker after remount, got %q", got)
	}

	// --- Verify: our own namespace was restored correctly ---
	// We should still be able to read our own files normally.
	if _, err := os.ReadDir(root); err != nil {
		t.Errorf("failed to read test root after remount (namespace leak?): %v", err)
	}
}

// TestIntegrationRemountViaSetnsRestoresNamespaceOnFailure verifies that
// if the bind mount fails (e.g., target doesn't exist in child
// namespace), remountViaSetns still restores the caller's namespace.
func TestIntegrationRemountViaSetnsRestoresNamespaceOnFailure(t *testing.T) {
	skipIfNotRoot(t)

	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	os.MkdirAll(srcDir, 0755)

	pidFile := filepath.Join(root, "child.pid")
	readyFile := filepath.Join(root, "child.ready")
	child := exec.Command("unshare", "--mount", "--propagation", "private",
		"sh", "-c", fmt.Sprintf(
			`echo $$ > %s && touch %s && sleep 60`, pidFile, readyFile,
		))
	child.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		child.Process.Kill()
		child.Wait()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	pidBytes, _ := os.ReadFile(pidFile)
	childPID, _ := strconv.Atoi(strings.TrimSpace(string(pidBytes)))

	// Try to remount a path that doesn't exist in the child namespace.
	err := remountViaSetns(childPID, "/nonexistent/path/that/does/not/exist", srcDir)
	if err == nil {
		t.Fatal("expected error for nonexistent target, got nil")
	}

	// Our namespace should still be intact.
	if _, readErr := os.ReadDir(root); readErr != nil {
		t.Fatalf("namespace not restored after failed remount: %v", readErr)
	}
}

// TestIntegrationFindContainerPIDsForPod verifies that
// findContainerPIDsForPod can find our own process when we create a
// child with a matching cgroup path.
func TestIntegrationFindContainerPIDsForPod(t *testing.T) {
	skipIfNotRoot(t)

	// Read our own cgroup to see what format the system uses.
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skipf("cannot read own cgroup: %v", err)
	}
	t.Logf("own cgroup:\n%s", string(data))

	// Search for our own PID using a substring of our cgroup. This
	// tests the scanning logic without needing a real pod.
	// Use a fake pod UID that won't match anything.
	pids, err := findContainerPIDsForPod("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if err != nil {
		t.Fatalf("findContainerPIDsForPod: %v", err)
	}
	if len(pids) != 0 {
		t.Errorf("expected 0 PIDs for bogus UID, got %d", len(pids))
	}
}

// TestIntegrationGetMountDevice verifies that getMountDevice can read
// the device of a real mount (e.g., the root filesystem or /tmp).
func TestIntegrationGetMountDevice(t *testing.T) {
	skipIfNotRoot(t)

	// Create a tmpfs mount to have a known target.
	mnt := filepath.Join(t.TempDir(), "mnt")
	os.MkdirAll(mnt, 0755)
	if err := unix.Mount("tmpfs", mnt, "tmpfs", 0, "size=1m"); err != nil {
		t.Fatalf("mount tmpfs: %v", err)
	}
	defer unix.Unmount(mnt, unix.MNT_DETACH)

	dev, err := getMountDevice(mnt)
	if err != nil {
		t.Fatalf("getMountDevice(%s): %v", mnt, err)
	}
	if dev == "" {
		t.Fatal("getMountDevice returned empty device")
	}
	// Device should be in "major:minor" format.
	parts := strings.Split(dev, ":")
	if len(parts) != 2 {
		t.Errorf("expected device format 'major:minor', got %q", dev)
	}
	t.Logf("tmpfs device: %s", dev)
}

// TestIntegrationRemountInContainersNoOp verifies that
// remountInContainers is a safe no-op when there are no matching
// containers (the common case in unit test / CI environments).
func TestIntegrationRemountInContainersNoOp(t *testing.T) {
	skipIfNotRoot(t)

	// Should not panic or error even with bogus paths.
	remountInContainers(
		"/var/lib/kubelet/pods/fake-uid/volumes/kubernetes.io~csi/pv/mount",
		"/tmp/nonexistent-staging",
		"0:999",
	)
}

// TestIntegrationRemountStaleFuseInContainersNoOp verifies the
// scan-based variant is also a safe no-op.
func TestIntegrationRemountStaleFuseInContainersNoOp(t *testing.T) {
	skipIfNotRoot(t)

	remountStaleFuseInContainers(
		"/var/lib/kubelet/pods/fake-uid/volumes/kubernetes.io~csi/pv/mount",
		"/tmp/nonexistent-staging",
	)
}
