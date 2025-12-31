package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/datalocality"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/driver"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	flag "github.com/seaweedfs/seaweedfs/weed/util/fla9"
)

var (
	components     = flag.String("components", "controller,node", "components to run, by default both controller and node")
	enableAttacher = flag.Bool("attacher", true, "enable attacher, by default enabled for backward compatibility")
	driverName     = flag.String("driverName", "seaweedfs-csi-driver", "CSI driver name, used by CSIDriver and StorageClass")

	filer             = flag.String("filer", "localhost:8888", "filer server")
	endpoint          = flag.String("endpoint", "unix://tmp/seaweedfs-csi.sock", "CSI endpoint to accept gRPC calls")
	mountEndpoint     = flag.String("mountEndpoint", "unix:///tmp/seaweedfs-mount.sock", "mount service endpoint")
	nodeID            = flag.String("nodeid", "", "node id")
	version           = flag.Bool("version", false, "Print the version and exit.")
	concurrentWriters = flag.Int("concurrentWriters", 128, "limit concurrent goroutine writers if not 0")
	concurrentReaders = flag.Int("concurrentReaders", 128, "limit concurrent chunk fetches for read operations")
	cacheCapacityMB   = flag.Int("cacheCapacityMB", 0, "local file chunk cache capacity in MB")
	cacheDir          = flag.String("cacheDir", os.TempDir(), "local cache directory for file chunks and meta data")
	uidMap            = flag.String("map.uid", "", "map local uid to uid on filer, comma-separated <local_uid>:<filer_uid>")
	gidMap            = flag.String("map.gid", "", "map local gid to gid on filer, comma-separated <local_gid>:<filer_gid>")
	dataCenter        = flag.String("dataCenter", "", "dataCenter this node is running in (locality-definition)")
	dataLocalityStr   = flag.String("dataLocality", "", "which volume-nodes pods will use for activity (one-of: 'write_preferLocalDc'). Requires used locality-definitions to be set")
	dataLocality      datalocality.DataLocality
)

func main() {

	flag.Parse()

	if *version {
		info, err := driver.GetVersionJSON()
		if err != nil {
			log.Fatalln(err.Error())
		}
		fmt.Println(info)
		os.Exit(0)
	}

	err := convertRequiredValues()
	if err != nil {
		glog.Error("Failed converting flag: ", err)
		os.Exit(1)
	}

	runNode := false
	runController := false
	for _, c := range strings.Split(*components, ",") {
		switch c {
		case "controller":
			runController = true
		case "node":
			runNode = true
		default:
			glog.Errorf("invalid component: %s", c)
			os.Exit(1)
		}
	}

	glog.Infof("will run node: %v, controller: %v, attacher: %v", runNode, runController, *enableAttacher)
	if !runNode && !runController {
		glog.Errorf("at least one component should be enabled: either controller or node (use --components=...)")
		os.Exit(1)
	}

	err = checkPreconditions(runNode)
	if err != nil {
		glog.Error("Precondition failed: ", err)
		os.Exit(1)
	}

	glog.Infof("connect to filer %s", *filer)

	drv := driver.NewSeaweedFsDriver(*driverName, *filer, *nodeID, *endpoint, *mountEndpoint, *enableAttacher)

	drv.RunNode = runNode
	drv.RunController = runController

	drv.ConcurrentWriters = *concurrentWriters
	drv.ConcurrentReaders = *concurrentReaders
	drv.CacheCapacityMB = *cacheCapacityMB
	drv.CacheDir = *cacheDir
	drv.UidMap = *uidMap
	drv.GidMap = *gidMap
	drv.DataCenter = *dataCenter
	drv.DataLocality = dataLocality

	drv.Run()
}

func convertRequiredValues() error {
	// Convert DataLocalityStr to DataLocality
	if *dataLocalityStr != "" {
		var ok bool
		dataLocality, ok = datalocality.FromString(*dataLocalityStr)
		if !ok {
			return fmt.Errorf("dataLocality invalid value")
		}
	}

	return nil
}

func checkPreconditions(runNode bool) error {
	if err := driver.CheckDataLocality(&dataLocality, dataCenter); err != nil {
		return err
	}

	if runNode {
		if len(*nodeID) == 0 {
			return fmt.Errorf("driver requires node id to be set, use -nodeid=")
		}
	}

	return nil
}
