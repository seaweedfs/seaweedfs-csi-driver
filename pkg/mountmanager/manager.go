package mountmanager

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"k8s.io/mount-utils"
)

var kubeMounter = mount.New("")

// Manager owns weed mount processes and exposes helpers to start and stop them.
type Manager struct {
	weedBinary string

	mu     sync.Mutex
	mounts map[string]*mountEntry
	locks  *keyMutex
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
		weedBinary: binary,
		mounts:     make(map[string]*mountEntry),
		locks:      newKeyMutex(),
	}
}

// Mount starts a weed mount process using the provided request.
func (m *Manager) Mount(req *MountRequest) (*MountResponse, error) {
	if req == nil {
		return nil, errors.New("mount request is nil")
	}
	if err := validateMountRequest(req); err != nil {
		return nil, err
	}

	lock := m.locks.get(req.VolumeID)
	lock.Lock()
	defer lock.Unlock()

	if entry := m.getMount(req.VolumeID); entry != nil {
		if entry.targetPath == req.TargetPath {
			glog.Infof("volume %s already mounted at %s", req.VolumeID, req.TargetPath)
			return &MountResponse{LocalSocket: entry.localSocket}, nil
		}
		return nil, fmt.Errorf("volume %s already mounted at %s", req.VolumeID, entry.targetPath)
	}

	entry, err := m.startMount(req)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.mounts[req.VolumeID] = entry
	m.mu.Unlock()

	glog.Infof("started weed mount process for volume %s at %s", req.VolumeID, req.TargetPath)
	return &MountResponse{LocalSocket: entry.localSocket}, nil
}

// Unmount terminates the weed mount process associated with the provided request.
func (m *Manager) Unmount(req *UnmountRequest) (*UnmountResponse, error) {
	if req == nil {
		return nil, errors.New("unmount request is nil")
	}
	if req.VolumeID == "" {
		return nil, errors.New("volumeId is required")
	}

	lock := m.locks.get(req.VolumeID)
	lock.Lock()
	defer lock.Unlock()

	// Use getMount first to check if mounted, only remove from state after cleanup succeeds
	entry := m.getMount(req.VolumeID)
	if entry == nil {
		glog.Infof("volume %s not mounted", req.VolumeID)
		return &UnmountResponse{}, nil
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
	m.removeMount(req.VolumeID)

	glog.Infof("stopped weed mount process for volume %s at %s", req.VolumeID, entry.targetPath)
	return &UnmountResponse{}, nil
}

func (m *Manager) getMount(volumeID string) *mountEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounts[volumeID]
}

func (m *Manager) removeMount(volumeID string) *mountEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.mounts[volumeID]
	delete(m.mounts, volumeID)
	m.locks.delete(volumeID)
	return entry
}

func (m *Manager) startMount(req *MountRequest) (*mountEntry, error) {
	targetPath := req.TargetPath
	if err := ensureTargetClean(targetPath); err != nil {
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

	process, err := startWeedMountProcess(m.weedBinary, args, targetPath, req.VolumeID)
	if err != nil {
		return nil, err
	}

	return &mountEntry{
		volumeID:    req.VolumeID,
		targetPath:  targetPath,
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
				return fmt.Errorf("failed to unmount corrupted mount %s: %w", targetPath, unmountErr)
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
	done   chan struct{}
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
		done:   make(chan struct{}),
	}

	go process.wait()

	if err := waitForMount(target, 10*time.Second); err != nil {
		_ = process.stop()
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

	time.Sleep(100 * time.Millisecond)
	_ = kubeMounter.Unmount(p.target)

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
