package driver

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"os/exec"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"k8s.io/utils/mount"
)

// Config holds values to configure the driver
type Config struct {
	// Region          string
	Filer string
}

type Mount interface {
	Unmount() error
}

type Mounter interface {
	Mount(target string) (Mount, error)
}

type fuseMount struct {
	path string
	cmd  *exec.Cmd

	finished chan struct{}
}

func newMounter(volumeID string, readOnly bool, driver *SeaweedFsDriver, volContext map[string]string) (Mounter, error) {
	path, ok := volContext["path"]
	if !ok {
		path = fmt.Sprintf("/buckets/%s", volumeID)
	}

	collection, ok := volContext["collection"]
	if !ok {
		collection = volumeID
	}

	return newSeaweedFsMounter(volumeID, path, collection, readOnly, driver, volContext)
}

func newFuseMount(path string, command string, args []string) (Mount, error) {
	cmd := exec.Command(command, args...)
	glog.V(0).Infof("Mounting fuse with command: %s and args: %s", command, args)

	// log fuse process messages - we need an easy way to investigate crashes in case it happens
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	err := cmd.Start()
	if err != nil {
		glog.Errorf("running weed mount: %v", err)
		return nil, fmt.Errorf("Error fuseMount command: %s\nargs: %s\nerror: %v", command, args, err)
	}

	m := &fuseMount{
		path: path,
		cmd:  cmd,

		finished: make(chan struct{}),
	}

	// avoid zombie processes
	go func() {
		if err := cmd.Wait(); err != nil {
			glog.Errorf("weed mount exit, pid: %d, path: %v, error: %v", cmd.Process.Pid, path, err)
		} else {
			glog.Infof("weed mount exit, pid: %d, path: %v", cmd.Process.Pid, path)
		}

		close(m.finished)
	}()

	if err = waitForMount(path, 10*time.Second); err != nil {
		glog.Errorf("weed mount timeout, pid: %d, path: %v", cmd.Process.Pid, path)

		_ = m.finish(time.Second * 10)
		return nil, err
	} else {
		return m, nil
	}
}

func (fm *fuseMount) finish(timeout time.Duration) error {
	// ignore error, just inform we want process to exit
	_ = fm.cmd.Process.Signal(syscall.Signal(1))

	if err := fm.waitFinished(timeout); err != nil {
		glog.Errorf("weed mount terminate timeout, pid: %d, path: %v", fm.cmd.Process.Pid, fm.path)
		_ = fm.cmd.Process.Kill()
		if err = fm.waitFinished(time.Second * 1); err != nil {
			glog.Errorf("weed mount kill timeout, pid: %d, path: %v", fm.cmd.Process.Pid, fm.path)
			return err
		}
	}

	return nil
}

func (fm *fuseMount) waitFinished(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return context.DeadlineExceeded
	case <-fm.finished:
		return nil
	}
}

func (fm *fuseMount) Unmount() error {
	m := mount.New("")

	if ok, err := m.IsLikelyNotMountPoint(fm.path); !ok || mount.IsCorruptedMnt(err) {
		if err := m.Unmount(fm.path); err != nil {
			return err
		}
	}

	return fm.finish(time.Second * 30)
}
