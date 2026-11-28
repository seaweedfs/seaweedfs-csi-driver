package mountmanager

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/mount-utils"
)

// TestWatchProcessExitRemovesStaleEntry verifies that the manager's
// background watcher clears a mount entry from its map when the weed
// mount process exits on its own. Without this, a future Mount call for
// the same target would falsely receive an "already mounted" no-op
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
	m.mounts["/tmp/test-target"] = entry

	go m.watchProcessExit("/tmp/test-target", entry)

	// Sanity: entry is still tracked while the process is alive.
	if got := m.getMount("/tmp/test-target"); got != entry {
		t.Fatalf("expected entry tracked before process exit, got %v", got)
	}

	// Simulate the weed mount process dying.
	close(process.exited)
	close(process.done)

	// The watcher should remove the entry; poll briefly to give it time.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if m.getMount("/tmp/test-target") == nil {
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
// Unmount/re-Mount racing on the same target slot.
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
	m.mounts["/tmp/test-target"] = newEntry

	watcherDone := make(chan struct{})
	go func() {
		m.watchProcessExit("/tmp/test-target", oldEntry)
		close(watcherDone)
	}()

	close(oldProcess.exited)
	close(oldProcess.done)

	select {
	case <-watcherDone:
	case <-time.After(1 * time.Second):
		t.Fatal("watcher did not finish")
	}

	if got := m.getMount("/tmp/test-target"); got != newEntry {
		t.Fatalf("expected new entry preserved, got %v", got)
	}
}

// withSleepProcesses makes Mount start a sleep process instead of weed,
// so stop(), wait() and the exit watcher run as they do in production.
// It returns the number of processes started.
func withSleepProcesses(t *testing.T) *atomic.Int32 {
	t.Helper()
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("needs the sleep command")
	}
	restoreMounter := withFakeMounter("", mount.NewFakeMounter(nil), func(string) error { return nil })
	origStart := startWeedMount

	var started atomic.Int32
	var mu sync.Mutex
	var processes []*weedMountProcess
	startWeedMount = func(_ string, _ []string, target string, _ string) (*weedMountProcess, error) {
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		p := &weedMountProcess{cmd: cmd, target: target, exited: make(chan struct{}), done: make(chan struct{})}
		go p.wait()
		started.Add(1)
		mu.Lock()
		processes = append(processes, p)
		mu.Unlock()
		return p, nil
	}

	t.Cleanup(func() {
		// wait() uses kubeMounter, so let every process finish before
		// the fakes are restored.
		mu.Lock()
		defer mu.Unlock()
		for _, p := range processes {
			_ = p.cmd.Process.Kill()
			<-p.done
		}
		startWeedMount = origStart
		restoreMounter()
	})
	return &started
}

// newMountRequest returns a request for volumeID with its own target,
// local socket and cache dir under dir.
func newMountRequest(dir, volumeID, name string) *MountRequest {
	return &MountRequest{
		VolumeID:    volumeID,
		TargetPath:  filepath.Join(dir, name, "target"),
		CacheDir:    filepath.Join(dir, name, "cache"),
		LocalSocket: filepath.Join(dir, name+".sock"),
		MountArgs:   []string{"mount"},
	}
}

func running(entry *mountEntry) bool {
	select {
	case <-entry.process.exited:
		return false
	default:
		return true
	}
}

func TestMountVolumeAtSeveralTargets(t *testing.T) {
	started := withSleepProcesses(t)
	m := NewManager(Config{})
	dir := t.TempDir()
	first := newMountRequest(dir, "vol-1", "a")
	second := newMountRequest(dir, "vol-1", "b")
	firstAgain := *first
	firstAgain.TargetPath += "/"

	for _, req := range []*MountRequest{first, second, &firstAgain} {
		resp, err := m.Mount(req)
		if err != nil {
			t.Fatalf("mount at %s: %v", req.TargetPath, err)
		}
		if resp.LocalSocket != req.LocalSocket {
			t.Fatalf("mount at %s answered socket %s, want %s", req.TargetPath, resp.LocalSocket, req.LocalSocket)
		}
	}
	if n := started.Load(); n != 2 {
		t.Fatalf("started %d weed mounts, want 2: mounting a volume again at its target must be a no-op", n)
	}

	if _, err := m.Unmount(&UnmountRequest{VolumeID: "vol-1", TargetPath: first.TargetPath}); err != nil {
		t.Fatalf("unmount %s: %v", first.TargetPath, err)
	}
	if m.getMount(first.TargetPath) != nil {
		t.Fatal("unmounted mount is still tracked")
	}
	if _, err := os.Stat(first.CacheDir); !os.IsNotExist(err) {
		t.Fatalf("cache dir of the unmounted mount was not removed: %v", err)
	}
	remaining := m.getMount(second.TargetPath)
	if remaining == nil || !running(remaining) {
		t.Fatal("unmounting one target stopped the volume's other mount")
	}
	if _, err := os.Stat(second.CacheDir); err != nil {
		t.Fatalf("cache dir of the other mount: %v", err)
	}

	// A request without a target, as older clients send, stops the
	// volume's only remaining mount.
	if _, err := m.Unmount(&UnmountRequest{VolumeID: "vol-1"}); err != nil {
		t.Fatalf("unmount by volume: %v", err)
	}
	if m.getMount(second.TargetPath) != nil || running(remaining) {
		t.Fatal("unmount by volume did not stop the remaining mount")
	}
}

func TestMountRefusesSharedSocketOrCacheDir(t *testing.T) {
	started := withSleepProcesses(t)
	m := NewManager(Config{})
	dir := t.TempDir()
	first := newMountRequest(dir, "vol-1", "a")
	if _, err := m.Mount(first); err != nil {
		t.Fatalf("mount: %v", err)
	}

	sharedSocket := newMountRequest(dir, "vol-1", "b")
	sharedSocket.LocalSocket = first.LocalSocket
	sharedCache := newMountRequest(dir, "vol-1", "c")
	sharedCache.CacheDir = first.CacheDir
	for _, req := range []*MountRequest{sharedSocket, sharedCache} {
		if _, err := m.Mount(req); err == nil {
			t.Fatalf("mount at %s sharing the running mount's socket or cache dir succeeded", req.TargetPath)
		}
		if m.getMount(req.TargetPath) != nil {
			t.Fatalf("refused mount at %s is tracked", req.TargetPath)
		}
	}
	if n := started.Load(); n != 1 {
		t.Fatalf("started %d weed mounts, want 1", n)
	}
	if entry := m.getMount(first.TargetPath); entry == nil || !running(entry) {
		t.Fatal("refused mounts disturbed the running mount")
	}
}

func TestMountRefusesTargetOfAnotherVolume(t *testing.T) {
	started := withSleepProcesses(t)
	m := NewManager(Config{})
	dir := t.TempDir()
	first := newMountRequest(dir, "vol-1", "a")
	if _, err := m.Mount(first); err != nil {
		t.Fatalf("mount: %v", err)
	}

	other := newMountRequest(dir, "vol-2", "b")
	other.TargetPath = first.TargetPath
	if _, err := m.Mount(other); err == nil {
		t.Fatal("mounting another volume at a mounted target succeeded")
	}
	if entry := m.getMount(first.TargetPath); entry == nil || entry.volumeID != "vol-1" || !running(entry) {
		t.Fatal("the mount at the target was replaced or stopped")
	}
	if n := started.Load(); n != 1 {
		t.Fatalf("started %d weed mounts, want 1", n)
	}
}

func TestUnmountWithoutTargetNeedsASingleMount(t *testing.T) {
	withSleepProcesses(t)
	m := NewManager(Config{})
	dir := t.TempDir()

	if _, err := m.Unmount(&UnmountRequest{VolumeID: "vol-1"}); err != nil {
		t.Fatalf("unmounting a volume that is not mounted: %v", err)
	}

	first := newMountRequest(dir, "vol-1", "a")
	second := newMountRequest(dir, "vol-1", "b")
	for _, req := range []*MountRequest{first, second} {
		if _, err := m.Mount(req); err != nil {
			t.Fatalf("mount at %s: %v", req.TargetPath, err)
		}
	}
	if _, err := m.Unmount(&UnmountRequest{VolumeID: "vol-1"}); err == nil {
		t.Fatal("unmount without a target succeeded for a volume mounted at two targets")
	}
	for _, req := range []*MountRequest{first, second} {
		if entry := m.getMount(req.TargetPath); entry == nil || !running(entry) {
			t.Fatalf("ambiguous unmount stopped the mount at %s", req.TargetPath)
		}
	}
}

func TestUnmountRefusesAnotherVolumesMount(t *testing.T) {
	withSleepProcesses(t)
	m := NewManager(Config{})
	first := newMountRequest(t.TempDir(), "vol-1", "a")
	if _, err := m.Mount(first); err != nil {
		t.Fatalf("mount: %v", err)
	}

	if _, err := m.Unmount(&UnmountRequest{VolumeID: "vol-2", TargetPath: first.TargetPath}); err == nil {
		t.Fatal("unmounting another volume's target succeeded")
	}
	if entry := m.getMount(first.TargetPath); entry == nil || !running(entry) {
		t.Fatal("the mount was stopped by an unmount for another volume")
	}
}

// An exited mount whose entry is not reaped yet must not keep its socket
// and cache dir, and must be dropped: unmounting it later would remove the
// cache dir that the new mount uses.
func TestMountDropsExitedMountSharingItsResources(t *testing.T) {
	started := withSleepProcesses(t)
	m := NewManager(Config{})
	dir := t.TempDir()
	old := newMountRequest(dir, "vol-1", "a")
	dead := &weedMountProcess{target: old.TargetPath, exited: make(chan struct{}), done: make(chan struct{})}
	close(dead.exited)
	m.mounts[old.TargetPath] = &mountEntry{
		volumeID:    "vol-1",
		targetPath:  old.TargetPath,
		cacheDir:    old.CacheDir,
		localSocket: old.LocalSocket,
		process:     dead,
	}
	// The post-exit cleanup is still running when the new mount arrives.
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(dead.done)
	}()

	req := newMountRequest(dir, "vol-1", "b")
	req.LocalSocket, req.CacheDir = old.LocalSocket, old.CacheDir
	if _, err := m.Mount(req); err != nil {
		t.Fatalf("mount reusing an exited mount's socket and cache dir: %v", err)
	}
	if m.getMount(old.TargetPath) != nil {
		t.Fatal("the exited mount is still tracked")
	}
	if entry := m.getMount(req.TargetPath); entry == nil || !running(entry) {
		t.Fatal("the new mount is not running")
	}
	if n := started.Load(); n != 1 {
		t.Fatalf("started %d weed mounts, want 1", n)
	}
}
