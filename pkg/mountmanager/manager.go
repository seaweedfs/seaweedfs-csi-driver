package mountmanager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/datalocality"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/mount_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
			glog.V(1).Infof("volume %s already mounted at %s", req.VolumeID, req.TargetPath)
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

	go m.watchMount(req.VolumeID, entry)

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
		glog.V(1).Infof("volume %s not mounted", req.VolumeID)
		return &UnmountResponse{}, nil
	}

	defer os.RemoveAll(entry.cacheDir)

	if err := entry.process.stop(); err != nil {
		return nil, err
	}

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

func (m *Manager) watchMount(volumeID string, entry *mountEntry) {
	<-entry.process.done
	m.mu.Lock()
	current, ok := m.mounts[volumeID]
	if ok && current == entry {
		delete(m.mounts, volumeID)
	}
	m.mu.Unlock()
	os.RemoveAll(entry.cacheDir)
}

func (m *Manager) startMount(req *MountRequest) (*mountEntry, error) {
	targetPath := req.TargetPath
	if err := ensureTargetClean(targetPath); err != nil {
		return nil, err
	}

	cacheBase := req.CacheDir
	if cacheBase == "" {
		cacheBase = os.TempDir()
	}
	cacheDir := filepath.Join(cacheBase, req.VolumeID)
	if err := os.MkdirAll(cacheDir, 0750); err != nil {
		return nil, fmt.Errorf("creating cache dir: %w", err)
	}

	localSocket := LocalSocketPath(req.VolumeID)
	args, err := buildMountArgs(req, targetPath, cacheDir, localSocket)
	if err != nil {
		return nil, err
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
	if len(req.Filers) == 0 {
		return errors.New("at least one filer is required")
	}
	return nil
}

func buildMountArgs(req *MountRequest, targetPath, cacheDir, localSocket string) ([]string, error) {
	volumeContext := req.VolumeContext
	if volumeContext == nil {
		volumeContext = map[string]string{}
	}

	path := volumeContext["path"]
	if path == "" {
		path = fmt.Sprintf("/buckets/%s", req.VolumeID)
	}

	collection := volumeContext["collection"]
	if collection == "" {
		collection = req.VolumeID
	}

	args := []string{
		"-logtostderr=true",
		"mount",
		"-dirAutoCreate=true",
		"-umask=000",
		fmt.Sprintf("-dir=%s", targetPath),
		fmt.Sprintf("-localSocket=%s", localSocket),
		fmt.Sprintf("-cacheDir=%s", cacheDir),
	}

	if req.ReadOnly {
		args = append(args, "-readOnly")
	}

	argsMap := map[string]string{
		"collection":         collection,
		"filer":              strings.Join(req.Filers, ","),
		"filer.path":         path,
		"cacheCapacityMB":    strconv.Itoa(req.CacheCapacityMB),
		"concurrentWriters":  strconv.Itoa(req.ConcurrentWriters),
		"map.uid":            req.UidMap,
		"map.gid":            req.GidMap,
		"disk":               "",
		"dataCenter":         "",
		"replication":        "",
		"ttl":                "",
		"chunkSizeLimitMB":   "",
		"volumeServerAccess": "",
		"readRetryTime":      "",
	}

	dataLocality := datalocality.None
	if req.DataLocality != "" {
		if dl, ok := datalocality.FromString(req.DataLocality); ok {
			dataLocality = dl
		} else {
			return nil, fmt.Errorf("invalid dataLocality: %s", req.DataLocality)
		}
	}

	if contextLocality, ok := volumeContext["dataLocality"]; ok {
		if dl, ok := datalocality.FromString(contextLocality); ok {
			dataLocality = dl
		} else {
			return nil, fmt.Errorf("invalid volumeContext dataLocality: %s", contextLocality)
		}
	}

	if err := checkDataLocality(&dataLocality, req.DataCenter); err != nil {
		return nil, err
	}

	switch dataLocality {
	case datalocality.Write_preferLocalDc:
		argsMap["dataCenter"] = req.DataCenter
	}

	parameterArgMap := map[string]string{
		"uidMap":    "map.uid",
		"gidMap":    "map.gid",
		"filerPath": "filer.path",
		"diskType":  "disk",
	}

	ignoredArgs := map[string]struct{}{"dataLocality": {}}

	for key, value := range volumeContext {
		if _, ignored := ignoredArgs[key]; ignored {
			continue
		}
		if mapped, ok := parameterArgMap[key]; ok {
			key = mapped
		}
		if _, ok := argsMap[key]; !ok {
			glog.Warningf("VolumeContext '%s' ignored", key)
			continue
		}
		if value != "" {
			argsMap[key] = value
		}
	}

	for key, value := range argsMap {
		if value == "" {
			continue
		}
		args = append(args, fmt.Sprintf("-%s=%s", key, value))
	}

	return args, nil
}

func checkDataLocality(dataLocality *datalocality.DataLocality, dataCenter string) error {
	if *dataLocality != datalocality.None && dataCenter == "" {
		return fmt.Errorf("dataLocality set, but dataCenter is empty")
	}
	return nil
}

// Configure updates mount-level settings, e.g. quota. Intended to be invoked by a trusted caller.
func (m *Manager) Configure(req *ConfigureRequest) (*ConfigureResponse, error) {
	if req == nil {
		return nil, errors.New("configure request is nil")
	}

	lock := m.locks.get(req.VolumeID)
	lock.Lock()
	defer lock.Unlock()

	entry := m.getMount(req.VolumeID)
	if entry == nil {
		m.locks.delete(req.VolumeID)
		return nil, fmt.Errorf("volume %s not mounted", req.VolumeID)
	}

	client, err := newMountProcessClient(entry.localSocket)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	if err := client.Configure(req.CollectionCapacity); err != nil {
		return nil, err
	}

	return &ConfigureResponse{}, nil
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
	if p.cmd.Process == nil {
		return nil
	}

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

type mountProcessClient struct {
	conn   *grpc.ClientConn
	client mount_pb.SeaweedMountClient
}

func newMountProcessClient(localSocket string) (*mountProcessClient, error) {
	target := fmt.Sprintf("passthrough:///unix://%s", localSocket)
	conn, err := grpc.Dial(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial mount socket %s: %w", localSocket, err)
	}

	return &mountProcessClient{
		conn:   conn,
		client: mount_pb.NewSeaweedMountClient(conn),
	}, nil
}

func (c *mountProcessClient) Close() error {
	return c.conn.Close()
}

func (c *mountProcessClient) Configure(capacity int64) error {
	if capacity == 1 {
		capacity = 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.client.Configure(ctx, &mount_pb.ConfigureRequest{CollectionCapacity: capacity})
	return err
}
