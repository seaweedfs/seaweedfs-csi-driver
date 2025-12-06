package mountmanager

import (
	"errors"
	"fmt"
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

	entry := m.removeMount(req.VolumeID)
	if entry == nil {
		glog.Infof("volume %s not mounted", req.VolumeID)
		return &UnmountResponse{}, nil
	}

	if ok, err := kubeMounter.IsMountPoint(entry.targetPath); ok || mount.IsCorruptedMnt(err) {
		if err = kubeMounter.Unmount(entry.targetPath); err != nil {
			return &UnmountResponse{}, err
		}
	}

	defer os.RemoveAll(entry.cacheDir)

	if err := entry.process.stop(); err != nil {
		return nil, err
	}

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
	if err := os.MkdirAll(cacheDir, 0750); err != nil {
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

	process, err := startWeedMountProcess(m.weedBinary, args, targetPath)
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
	isMount, err := kubeMounter.IsMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(targetPath, 0750)
		}
		if mount.IsCorruptedMnt(err) {
			if err := kubeMounter.Unmount(targetPath); err != nil {
				return err
			}
			return ensureTargetClean(targetPath)
		}
		return err
	}
	if isMount {
		if err := kubeMounter.Unmount(targetPath); err != nil {
			return err
		}
	}
	return nil
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

func startWeedMountProcess(command string, args []string, target string) (*weedMountProcess, error) {
	cmd := exec.Command(command, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	glog.V(0).Infof("Starting weed mount: %s %s", command, strings.Join(args, " "))

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting weed mount: %w", err)
	}

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
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil && err != os.ErrProcessDone {
		glog.Warningf("sending SIGTERM to weed mount failed: %v", err)
	}

	select {
	case <-p.done:
		return nil
	case <-time.After(5 * time.Second):
	}

	if err := p.cmd.Process.Kill(); err != nil && err != os.ErrProcessDone {
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
