package mountmanager

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"k8s.io/mount-utils"
)

var kubeMounter = mount.New("")

var lazyUnmount = LazyUnmount

var startWeedMount = startWeedMountProcess

// Manager owns weed mount processes and exposes helpers to start and stop them.
//
// Mounts are keyed by target path, so a volume can be mounted at several
// targets at once, each by its own weed mount process. Operations on one
// volume are serialized, and so are operations on one target; the volume
// lock is always taken before the target lock.
type Manager struct {
	weedBinary string

	mu          sync.Mutex
	mounts      map[string]*mountEntry // keyed by the cleaned target path
	volumeLocks *keyMutex
	targetLocks *keyMutex
}

// Config configures a Manager instance.
type Config struct {
	WeedBinary string
}

// NewManager returns a Manager ready to accept mount requests.
func NewManager(cfg Config) *Manager {
	binary := cfg.WeedBinary
	if binary == "" {
		binary = DefaultWeedBinary
	}
	return &Manager{
		weedBinary:  binary,
		mounts:      make(map[string]*mountEntry),
		volumeLocks: newKeyMutex(),
		targetLocks: newKeyMutex(),
	}
}

// Mount starts a weed mount process using the provided request. Mounting a
// volume again at the target where it is mounted is a no-op. A volume can
// also be mounted at other targets, but each of its mounts needs its own
// local socket and cache directory.
func (m *Manager) Mount(req *MountRequest) (*MountResponse, error) {
	if req == nil {
		return nil, errors.New("mount request is nil")
	}
	if err := validateMountRequest(req); err != nil {
		return nil, err
	}
	target := filepath.Clean(req.TargetPath)

	volumeLock := m.volumeLocks.get(req.VolumeID)
	volumeLock.Lock()
	defer volumeLock.Unlock()
	targetLock := m.targetLocks.get(target)
	targetLock.Lock()
	defer targetLock.Unlock()

	if entry := m.getMount(target); entry != nil {
		// If the previous weed mount process has died, the entry is
		// stale and the FUSE mount is dead. Tear down the stale entry
		// so we can start a fresh process; otherwise we would falsely
		// report "already mounted" and a CSI-driver recovery would
		// silently bind-mount onto a dead path. See seaweedfs/seaweedfs-csi-driver#261.
		select {
		case <-entry.process.exited:
			glog.Warningf("volume %s previous weed mount process at %s has exited; replacing stale entry with a fresh mount", entry.volumeID, target)
			// Wait for wait()'s post-exit cleanup (FUSE unmount) to finish
			// so ensureTargetClean below sees a quiescent path.
			<-entry.process.done
			m.removeMount(target, entry)
		default:
			if entry.volumeID != req.VolumeID {
				return nil, fmt.Errorf("target %s is already mounted by volume %s", target, entry.volumeID)
			}
			glog.Infof("volume %s already mounted at %s", req.VolumeID, target)
			return &MountResponse{LocalSocket: entry.localSocket}, nil
		}
	}

	if err := m.reclaimResources(req); err != nil {
		return nil, err
	}

	entry, err := m.startMount(req, target)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.mounts[target] = entry
	m.mu.Unlock()

	// Proactively clear the entry once the weed mount process exits,
	// even if Unmount is never called. Without this, a process that
	// crashes on its own (e.g. backend errors) leaves a stale entry
	// that fools later Mount calls into reporting success without
	// starting a new process.
	go m.watchProcessExit(target, entry)

	glog.Infof("started weed mount process for volume %s at %s", req.VolumeID, target)
	return &MountResponse{LocalSocket: entry.localSocket}, nil
}

// reclaimResources makes sure that no running mount of this service uses the
// local socket or the cache directory of req. A second weed mount would take
// the socket over from the first one, and unmounting either of them removes
// the shared cache directory. Entries whose process has already exited are
// dropped, as Mount does with a stale entry at its own target. The caller
// holds the volume lock of req, so the check is atomic against the volume's
// own mounts; different volumes do not share these paths, because the node
// plugin derives both from the volume ID.
func (m *Manager) reclaimResources(req *MountRequest) error {
	socket := filepath.Clean(req.LocalSocket)
	cacheDir := filepath.Clean(req.CacheDir)

	type staleMount struct {
		target string
		entry  *mountEntry
	}
	var stale []staleMount

	m.mu.Lock()
	for target, entry := range m.mounts {
		var resource string
		switch {
		case filepath.Clean(entry.localSocket) == socket:
			resource = "local socket " + socket
		case filepath.Clean(entry.cacheDir) == cacheDir:
			resource = "cache dir " + cacheDir
		default:
			continue
		}
		select {
		case <-entry.process.exited:
			stale = append(stale, staleMount{target: target, entry: entry})
		default:
			m.mu.Unlock()
			return fmt.Errorf("volume %s mounted at %s already uses %s; each mount needs its own local socket and cache dir", entry.volumeID, target, resource)
		}
	}
	m.mu.Unlock()

	for _, s := range stale {
		glog.Warningf("volume %s previous weed mount process at %s has exited; dropping its entry to reuse its local socket and cache dir", s.entry.volumeID, s.target)
		<-s.entry.process.done
		m.removeMount(s.target, s.entry)
	}
	return nil
}

// watchProcessExit removes the entry at target once its weed mount
// process has finished exiting. It is safe to run concurrently with
// Unmount: if Unmount has already removed the entry (or replaced it
// with a fresh mount), the identity check ensures we leave the new
// entry alone.
func (m *Manager) watchProcessExit(target string, entry *mountEntry) {
	<-entry.process.done
	if m.removeMount(target, entry) {
		glog.Infof("removed mount entry for volume %s at %s after weed mount process exited", entry.volumeID, target)
	}
}

// Unmount terminates the weed mount process at the requested target. A
// request without a target, as older clients send, stops the volume's only
// mount and fails if the volume is mounted at several targets.
func (m *Manager) Unmount(req *UnmountRequest) (*UnmountResponse, error) {
	if req == nil {
		return nil, errors.New("unmount request is nil")
	}
	if req.VolumeID == "" {
		return nil, errors.New("volumeId is required")
	}

	volumeLock := m.volumeLocks.get(req.VolumeID)
	volumeLock.Lock()
	defer volumeLock.Unlock()

	target := req.TargetPath
	if target == "" {
		targets := m.targetsOf(req.VolumeID)
		switch len(targets) {
		case 0:
			glog.Infof("volume %s not mounted", req.VolumeID)
			return &UnmountResponse{}, nil
		case 1:
			target = targets[0]
		default:
			return nil, fmt.Errorf("volume %s is mounted at %s; the unmount request must name the targetPath", req.VolumeID, strings.Join(targets, ", "))
		}
	}
	target = filepath.Clean(target)

	targetLock := m.targetLocks.get(target)
	targetLock.Lock()
	defer targetLock.Unlock()

	// Use getMount first to check if mounted, only remove from state after cleanup succeeds
	entry := m.getMount(target)
	if entry == nil {
		glog.Infof("volume %s not mounted at %s", req.VolumeID, target)
		return &UnmountResponse{}, nil
	}
	if entry.volumeID != req.VolumeID {
		if req.TargetPath == "" {
			// The volume's only mount was at target and has been replaced
			// since targetsOf ran; nothing of the volume is left to stop.
			glog.Infof("volume %s not mounted", req.VolumeID)
			return &UnmountResponse{}, nil
		}
		return nil, fmt.Errorf("target %s is mounted by volume %s, not %s", target, entry.volumeID, req.VolumeID)
	}

	// Note: We don't explicitly unmount here because weedMountProcess.wait()
	// handles the unmount when the process terminates (either gracefully or forcefully).
	// This centralizes unmount logic and avoids potential race conditions.
	if err := entry.process.stop(); err != nil {
		return nil, err
	}

	// Remove cache dir only after process has been successfully stopped
	if err := os.RemoveAll(entry.cacheDir); err != nil {
		glog.Warningf("failed to remove cache dir %s for volume %s: %v", entry.cacheDir, req.VolumeID, err)
	}

	// Only remove from state after all cleanup operations succeeded
	m.removeMount(target, entry)

	glog.Infof("stopped weed mount process for volume %s at %s", req.VolumeID, target)
	return &UnmountResponse{}, nil
}

func (m *Manager) getMount(target string) *mountEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounts[target]
}

// targetsOf returns the sorted targets at which volumeID is mounted.
func (m *Manager) targetsOf(volumeID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var targets []string
	for target, entry := range m.mounts {
		if entry.volumeID == volumeID {
			targets = append(targets, target)
		}
	}
	slices.Sort(targets)
	return targets
}

// removeMount drops the entry at target if it is still the given entry, and
// reports whether it did.
func (m *Manager) removeMount(target string, entry *mountEntry) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mounts[target] != entry {
		return false
	}
	delete(m.mounts, target)
	// Intentionally do NOT delete the per-volume or per-target locks
	// here. If a caller is still holding a lock from m.volumeLocks.get or
	// m.targetLocks.get, deleting it would let a concurrent caller receive
	// a brand-new lock from the next get() and enter the critical section
	// in parallel — splitting Mount/Unmount serialization for the same
	// volume or target. The lock maps grow by one small entry per volume and
	// per target ever requested, which is negligible for the manager's
	// lifetime.
	return true
}

func (m *Manager) startMount(req *MountRequest, target string) (*mountEntry, error) {
	if err := ensureTargetClean(target); err != nil {
		return nil, err
	}

	cacheDir := req.CacheDir
	if cacheDir == "" {
		return nil, errors.New("cacheDir is required")
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("creating cache dir: %w", err)
	}

	localSocket := req.LocalSocket
	if localSocket == "" {
		return nil, errors.New("localSocket is required")
	}

	args := req.MountArgs
	if len(args) == 0 {
		return nil, errors.New("mountArgs is required")
	}

	process, err := startWeedMount(m.weedBinary, args, target, req.VolumeID)
	if err != nil {
		return nil, err
	}

	return &mountEntry{
		volumeID:    req.VolumeID,
		targetPath:  target,
		cacheDir:    cacheDir,
		localSocket: localSocket,
		process:     process,
	}, nil
}

func ensureTargetClean(targetPath string) error {
	// Use IsLikelyNotMountPoint instead of deprecated IsMountPoint
	notMnt, err := kubeMounter.IsLikelyNotMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Path does not exist, which is a clean state. Directory will be created below.
		} else if mount.IsCorruptedMnt(err) {
			glog.Warningf("Target path %s is a corrupted mount, attempting to unmount", targetPath)
			if unmountErr := kubeMounter.Unmount(targetPath); unmountErr != nil {
				glog.Warningf("unmount corrupted mount %s failed: %v; trying lazy unmount", targetPath, unmountErr)
				if lazyErr := lazyUnmount(targetPath); lazyErr != nil {
					return fmt.Errorf("failed to unmount corrupted mount %s: regular unmount: %v; lazy unmount: %w", targetPath, unmountErr, lazyErr)
				}
			}
		} else {
			return err
		}
	} else if !notMnt {
		glog.Infof("Target path %s is an existing mount, attempting to unmount", targetPath)
		if unmountErr := kubeMounter.Unmount(targetPath); unmountErr != nil {
			return fmt.Errorf("failed to unmount existing mount %s: %w", targetPath, unmountErr)
		}
	}

	// Ensure the path exists and is a directory.
	return os.MkdirAll(targetPath, 0755)
}

func validateMountRequest(req *MountRequest) error {
	if req.VolumeID == "" {
		return errors.New("volumeId is required")
	}
	if req.TargetPath == "" {
		return errors.New("targetPath is required")
	}
	if req.CacheDir == "" {
		return errors.New("cacheDir is required")
	}
	if req.LocalSocket == "" {
		return errors.New("localSocket is required")
	}
	if len(req.MountArgs) == 0 {
		return errors.New("mountArgs is required")
	}
	return nil
}

type mountEntry struct {
	volumeID    string
	targetPath  string
	cacheDir    string
	localSocket string
	process     *weedMountProcess
}

type weedMountProcess struct {
	cmd    *exec.Cmd
	target string
	// exited is closed as soon as cmd.Wait() returns, so callers can
	// detect that the weed mount process is gone without waiting for
	// the post-exit FUSE unmount step.
	exited chan struct{}
	// done is closed after wait() finishes its full cleanup (including
	// the kubeMounter.Unmount of the target).
	done chan struct{}
}

func startWeedMountProcess(command string, args []string, target string, volumeID string) (*weedMountProcess, error) {
	cmd := exec.Command(command, args...)

	// Capture stdout/stderr and log with volume ID prefix for better debugging
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stderr pipe: %w", err)
	}

	glog.V(0).Infof("[%s] Starting weed mount: %s %s", volumeID, command, strings.Join(args, " "))

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting weed mount: %w", err)
	}

	// Forward stdout/stderr with volume ID prefix for better debugging
	go forwardLogs(stdoutPipe, volumeID, "stdout")
	go forwardLogs(stderrPipe, volumeID, "stderr")

	process := &weedMountProcess{
		cmd:    cmd,
		target: target,
		exited: make(chan struct{}),
		done:   make(chan struct{}),
	}

	go process.wait()

	if err := waitForMount(target, 10*time.Second); err != nil {
		if stopErr := process.stop(); stopErr != nil {
			glog.Warningf("[%s] failed to stop mount process after mount wait failure: %v", volumeID, stopErr)
		}
		return nil, err
	}

	return process, nil
}

func (p *weedMountProcess) wait() {
	if err := p.cmd.Wait(); err != nil {
		glog.Errorf("weed mount exit (pid: %d, target: %s): %v", p.cmd.Process.Pid, p.target, err)
	} else {
		glog.Infof("weed mount exit (pid: %d, target: %s)", p.cmd.Process.Pid, p.target)
	}

	// Signal exit immediately so Manager.Mount can detect a dead
	// process without waiting for the post-exit unmount step.
	close(p.exited)

	// Brief delay to allow FUSE cleanup and pending I/O to complete before unmounting
	time.Sleep(100 * time.Millisecond)
	if err := kubeMounter.Unmount(p.target); err != nil {
		glog.Warningf("umount %s after weed mount exit failed: %v; trying lazy unmount", p.target, err)
		if lazyErr := lazyUnmount(p.target); lazyErr != nil {
			glog.Warningf("lazy unmount %s after weed mount exit failed: %v", p.target, lazyErr)
		}
	}

	close(p.done)
}

func (p *weedMountProcess) stop() error {
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		glog.Warningf("sending SIGTERM to weed mount failed: %v", err)
	}

	select {
	case <-p.done:
		return nil
	case <-time.After(5 * time.Second):
	}

	if err := p.cmd.Process.Kill(); err != nil {
		glog.Warningf("killing weed mount failed: %v", err)
	}

	select {
	case <-p.done:
		return nil
	case <-time.After(1 * time.Second):
		return errors.New("timed out waiting for weed mount to stop")
	}
}

func waitForMount(path string, timeout time.Duration) error {
	var elapsed time.Duration
	interval := 10 * time.Millisecond

	for {
		notMount, err := kubeMounter.IsLikelyNotMountPoint(path)
		if err != nil {
			return err
		}
		if !notMount {
			return nil
		}

		time.Sleep(interval)
		elapsed += interval
		if elapsed >= timeout {
			return errors.New("timeout waiting for mount")
		}
	}
}

// forwardLogs reads from a pipe and logs each line with a volume ID prefix.
func forwardLogs(pipe io.ReadCloser, volumeID string, stream string) {
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		glog.Infof("[%s] %s: %s", volumeID, stream, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		glog.Warningf("[%s] error reading %s: %v", volumeID, stream, err)
	}
}
