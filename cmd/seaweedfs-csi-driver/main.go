package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/driver"
)

var (
	filer             = flag.String("filer", "localhost:8888", "filer server")
	endpoint          = flag.String("endpoint", "unix://tmp/seaweedfs-csi.sock", "CSI endpoint to accept gRPC calls")
	nodeID            = flag.String("nodeid", "", "node id")
	version           = flag.Bool("version", false, "Print the version and exit.")
	concurrentWriters = flag.Int("concurrentWriters", 32, "limit concurrent goroutine writers if not 0")
	cacheSizeMB       = flag.Int64("cacheCapacityMB", 1000, "local file chunk cache capacity in MB (0 will disable cache)")
	cacheDir          = flag.String("cacheDir", os.TempDir(), "local cache directory for file chunks and meta data")
	uidMap            = flag.String("map.uid", "", "map local uid to uid on filer, comma-separated <local_uid>:<filer_uid>")
	gidMap            = flag.String("map.gid", "", "map local gid to gid on filer, comma-separated <local_gid>:<filer_gid>")
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

	glog.Infof("connect to filer %s", *filer)

	drv := driver.NewSeaweedFsDriver(*filer, *nodeID, *endpoint)
	drv.ConcurrentWriters = *concurrentWriters
	drv.CacheSizeMB = *cacheSizeMB
	drv.CacheDir = *cacheDir
	drv.UidMap = *uidMap
	drv.GidMap = *gidMap
	drv.Run()
}
