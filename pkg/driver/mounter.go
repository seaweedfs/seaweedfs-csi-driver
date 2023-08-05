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

type Unmounter interface {
	Unmount() error
}

type Mounter interface {
	Mount(target string) (Unmounter, error)
}

type fuseUnmounter struct {
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

func fuseMount(path string, command string, args []string) (Unmounter, error) {
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

	fu := &fuseUnmounter{
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

		// make sure we'll have no stale mounts
		time.Sleep(time.Millisecond * 100)
		_ = mount.New("").Unmount(path)

		close(fu.finished)
	}()

	if err = waitForMount(path, 10*time.Second); err != nil {
		glog.Errorf("weed mount timeout, pid: %d, path: %v", cmd.Process.Pid, path)

		_ = fu.finish(time.Second * 10)
		return nil, err
	} else {
		return fu, nil
	}
}

func (fu *fuseUnmounter) finish(timeout time.Duration) error {
	// ignore error, just inform we want process to exit
	// SIGHUP is used to reload weed config - we need to use SIGTERM
	_ = fu.cmd.Process.Signal(syscall.SIGTERM)

	if err := fu.waitFinished(timeout); err != nil {
		glog.Errorf("weed mount terminate timeout, pid: %d, path: %v", fu.cmd.Process.Pid, fu.path)
		_ = fu.cmd.Process.Kill()
		if err = fu.waitFinished(time.Second * 1); err != nil {
			glog.Errorf("weed mount kill timeout, pid: %d, path: %v", fu.cmd.Process.Pid, fu.path)
			return err
		}
	}

	return nil
}

func (fu *fuseUnmounter) waitFinished(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return context.DeadlineExceeded
	case <-fu.finished:
		return nil
	}
}

func (fu *fuseUnmounter) Unmount() error {
	m := mount.New("")

	if ok, err := mount.IsNotMountPoint(m, fu.path); !ok || mount.IsCorruptedMnt(err) {
		if err := m.Unmount(fu.path); err != nil {
			return err
		}
	}

	return fu.finish(time.Second * 5)
}
