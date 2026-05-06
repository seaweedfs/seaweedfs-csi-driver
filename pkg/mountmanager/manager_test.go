package mountmanager

import (
	"testing"
	"time"
)

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

	if got := m.getMount("vol-1"); got != entry {
		t.Fatalf("expected entry tracked before process exit, got %v", got)
	}

	close(process.exited)
	close(process.done)

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if m.getMount("vol-1") == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("watcher did not remove stale entry after weed mount process exited")
}

// Identity check in watchProcessExit must leave a fresh replacement
// entry alone when a previous process's watcher fires.
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

	m.mounts["vol-1"] = newEntry

	watcherDone := make(chan struct{})
	go func() {
		m.watchProcessExit("vol-1", oldEntry)
		close(watcherDone)
	}()

	close(oldProcess.exited)
	close(oldProcess.done)

	select {
	case <-watcherDone:
	case <-time.After(1 * time.Second):
		t.Fatal("watcher did not finish")
	}

	if got := m.getMount("vol-1"); got != newEntry {
		t.Fatalf("expected new entry preserved, got %v", got)
	}
}
