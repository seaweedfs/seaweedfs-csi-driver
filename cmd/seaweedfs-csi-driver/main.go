package main

import (
	"fmt"
	"log"
	"os"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/datalocality"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/driver"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	flag "github.com/seaweedfs/seaweedfs/weed/util/fla9"
)

var (
	filer             = flag.String("filer", "localhost:8888", "filer server")
	endpoint          = flag.String("endpoint", "unix://tmp/seaweedfs-csi.sock", "CSI endpoint to accept gRPC calls")
	nodeID            = flag.String("nodeid", "", "node id")
	version           = flag.Bool("version", false, "Print the version and exit.")
	concurrentWriters = flag.Int("concurrentWriters", 32, "limit concurrent goroutine writers if not 0")
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

	err = checkPreconditions()
	if err != nil {
		glog.Error("Precondition failed: ", err)
		os.Exit(1)
	}

	glog.Infof("connect to filer %s", *filer)

	drv := driver.NewSeaweedFsDriver(*filer, *nodeID, *endpoint)
	drv.ConcurrentWriters = *concurrentWriters
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

func checkPreconditions() error {
	if err := driver.CheckDataLocality(&dataLocality, dataCenter); err != nil {
		return err
	}

	if len(*nodeID) == 0 {
		return fmt.Errorf("driver requires node id to be set, use -nodeid=")
	}

	return nil
}
