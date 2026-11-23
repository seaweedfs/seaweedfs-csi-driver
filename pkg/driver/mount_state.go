package driver

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

type mountState struct {
	VolumeID     string            `json:"volumeId"`
	StagingPath  string            `json:"stagingPath"`
	Context      map[string]string `json:"volumeContext"`
	ReadOnly     bool              `json:"readOnly"`
	PublishPaths map[string]bool   `json:"publishPaths,omitempty"`
}

func mountStateName(volumeID string) string {
	return fmt.Sprintf("%x.json", sha256.Sum256([]byte(volumeID)))
}

// saveMountState is called under the volume lock. An atomic rename lets a
// replacement node plugin read a complete record even if the old Pod exits
// while changing the mount.
func (ns *NodeServer) saveMountState(vol *Volume) error {
	dir := ns.Driver.MountStateDir
	if dir == "" {
		return nil
	}
	state := mountState{
		VolumeID:     vol.VolumeId,
		StagingPath:  vol.StagedPath,
		Context:      vol.volContext,
		ReadOnly:     vol.readOnly,
		PublishPaths: make(map[string]bool),
	}
	vol.publishPaths.Range(func(path, readOnly any) bool {
		state.PublishPaths[path.(string)] = readOnly.(bool)
		return true
	})

	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".mount-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	encodeErr := json.NewEncoder(f).Encode(state)
	if err := errors.Join(encodeErr, f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, mountStateName(vol.VolumeId)))
}

func (ns *NodeServer) removeMountState(volumeID string) error {
	if ns.Driver.MountStateDir == "" {
		return nil
	}
	err := os.Remove(filepath.Join(ns.Driver.MountStateDir, mountStateName(volumeID)))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// restoreMountState indexes the mount table once, without reading from FUSE.
// A dead but still attached mount must be tracked for the monitor to repair it.
func (ns *NodeServer) restoreMountState(listMounts func() (map[string]string, error)) error {
	dir := ns.Driver.MountStateDir
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	entries = slices.DeleteFunc(entries, func(entry os.DirEntry) bool {
		return !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json")
	})
	if len(entries) == 0 {
		return nil
	}
	mounts, err := listMounts()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			glog.Warningf("mount state: cannot read %s: %v", entry.Name(), err)
			continue
		}
		var state mountState
		if err := json.Unmarshal(data, &state); err != nil || state.VolumeID == "" ||
			state.StagingPath == "" ||
			entry.Name() != mountStateName(state.VolumeID) || state.Context == nil {
			glog.Warningf("mount state: invalid record %s", entry.Name())
			continue
		}
		stagingPath := filepath.Clean(state.StagingPath)
		// A target keeps a dead or older FUSE bind when recovery failed
		// part way; tracking it lets the health monitor bind it again.
		published := make(map[string]bool)
		for target, readOnly := range state.PublishPaths {
			target = filepath.Clean(target)
			if mounts[target] != "" {
				published[target] = readOnly
			}
		}
		if mounts[stagingPath] == "" && len(published) == 0 {
			// A journal may outlive a node reboot or an unstage. Never
			// recreate a staging mount from a record alone.
			continue
		}
		vol, err := ns.rebuildVolumeFromStaging(state.VolumeID, stagingPath)
		if err != nil {
			return err
		}
		vol.volContext = state.Context
		vol.readOnly = state.ReadOnly
		vol.stagingLost = mounts[stagingPath] == ""
		for target, readOnly := range published {
			vol.AddPublishPath(target, readOnly)
		}
		ns.volumes.Store(state.VolumeID, vol)
		glog.Infof("mount state: restored tracking for volume %s at %s", state.VolumeID, stagingPath)
	}
	return nil
}
