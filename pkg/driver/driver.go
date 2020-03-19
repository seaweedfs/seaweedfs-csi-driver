package driver

import (
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/glog"
)

const (
	driverName = "csi.seaweedfs.com"
)

var (
	version = "1.0.0-rc1"
)

type SeaweedFsDriver struct {
	name    string
	nodeID  string
	version string

	endpoint string

	vcap  []*csi.VolumeCapability_AccessMode
	cscap []*csi.ControllerServiceCapability

	filer       string
	pathOnFiler string
}

func NewSeaweedFsDriver(nodeID, endpoint string) *SeaweedFsDriver {

	glog.Infof("Driver: %v version: %v", driverName, version)

	n := &SeaweedFsDriver{
		endpoint: endpoint,
		nodeID:   nodeID,
		name:     driverName,
		version:  version,
	}

	n.AddVolumeCapabilityAccessModes([]csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
	})
	n.AddControllerServiceCapabilities([]csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	})

	return n
}

func NewNodeServer(n *SeaweedFsDriver) *NodeServer {

	return &NodeServer{
		Driver: n,
	}
}

func (n *SeaweedFsDriver) Run() {
	s := NewNonBlockingGRPCServer()
	s.Start(n.endpoint,
		NewIdentityServer(n),
		NewControllerServer(n),
		NewNodeServer(n))
	s.Wait()
}

func (n *SeaweedFsDriver) AddVolumeCapabilityAccessModes(vc []csi.VolumeCapability_AccessMode_Mode) []*csi.VolumeCapability_AccessMode {
	var vca []*csi.VolumeCapability_AccessMode
	for _, c := range vc {
		glog.Infof("Enabling volume access mode: %v", c.String())
		vca = append(vca, &csi.VolumeCapability_AccessMode{Mode: c})
	}
	n.vcap = vca
	return vca
}

func (n *SeaweedFsDriver) AddControllerServiceCapabilities(cl []csi.ControllerServiceCapability_RPC_Type) {
	var csc []*csi.ControllerServiceCapability

	for _, c := range cl {
		glog.Infof("Enabling controller service capability: %v", c.String())
		csc = append(csc, NewControllerServiceCapability(c))
	}

	n.cscap = csc

	return
}

func (n *SeaweedFsDriver) createBucket(volumeId string, seaweedFsVolumeCount int) error {
	// TODO implement seaweedFsVolumeCount later
	return nil
}
func (n *SeaweedFsDriver) deleteBucket(volumeId string) error {
	return nil
}
func (n *SeaweedFsDriver) mount(source string, targetPath string) error {
	return nil
}
func (n *SeaweedFsDriver) unmount(targetPath string) error {
	return nil
}
