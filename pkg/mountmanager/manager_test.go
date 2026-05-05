package mountmanager

import (
	"testing"
	"time"
)

// TestWatchProcessExitRemovesStaleEntry verifies that the manager's
// background watcher clears a mount entry from its map when the weed
// mount process exits on its own. Without this, a future Mount call for
// the same volume would falsely receive an "already mounted" no-op
// because the manager's state never noticed the dead process.
// Regression test for seaweedfs/seaweedfs-csi-driver#261.
func TestWatchProcessExitRemovesStaleEntry(t *testing.T) {
	m := NewManager(Config{})

	process := &weedMountProcess{
		target: "/tmp/test-target",
		exited: make(chan struct{}),
		done:   make(chan struct{}),
	}
	entry := &mountEntry{
		volumeID:    "vol-1",
		targetPath:  "/tmp/test-target",
		cacheDir:    "/tmp/test-cache",
		localSocket: "/tmp/test.sock",
		process:     process,
	}
	m.mounts["vol-1"] = entry

	go m.watchProcessExit("vol-1", entry)

	// Sanity: entry is still tracked while the process is alive.
	if got := m.getMount("vol-1"); got != entry {
		t.Fatalf("expected entry tracked before process exit, got %v", got)
	}

	// Simulate the weed mount process dying.
	close(process.exited)
	close(process.done)

	// The watcher should remove the entry; poll briefly to give it time.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if m.getMount("vol-1") == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("watcher did not remove stale entry after weed mount process exited")
}

// TestWatchProcessExitLeavesReplacedEntry verifies that if a fresh
// mount has replaced the original entry by the time the watcher fires,
// the watcher does not delete the new entry. The identity check in
// watchProcessExit guards against the watcher and a concurrent
// Unmount/re-Mount racing on the same volumeID slot.
func TestWatchProcessExitLeavesReplacedEntry(t *testing.T) {
	m := NewManager(Config{})

	oldProcess := &weedMountProcess{
		target: "/tmp/test-target",
		exited: make(chan struct{}),
		done:   make(chan struct{}),
	}
	oldEntry := &mountEntry{
		volumeID: "vol-1",
		process:  oldProcess,
	}

	newProcess := &weedMountProcess{
		target: "/tmp/test-target",
		exited: make(chan struct{}),
		done:   make(chan struct{}),
	}
	newEntry := &mountEntry{
		volumeID: "vol-1",
		process:  newProcess,
	}

	// Put the new entry in the map (as if a fresh mount had replaced
	// the old one) before kicking off the watcher for the old entry.
	m.mounts["vol-1"] = newEntry

	go m.watchProcessExit("vol-1", oldEntry)

	close(oldProcess.exited)
	close(oldProcess.done)

	// Give the watcher a moment to run.
	time.Sleep(50 * time.Millisecond)

	if got := m.getMount("vol-1"); got != newEntry {
		t.Fatalf("expected new entry preserved, got %v", got)
	}
}
