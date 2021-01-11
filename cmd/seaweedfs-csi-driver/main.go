package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/driver"
)

var (
	filer             = flag.String("filer", "localhost:8888", "filer server")
	endpoint          = flag.String("endpoint", "unix://tmp/seaweedfs-csi.sock", "CSI endpoint to accept gRPC calls")
	nodeID            = flag.String("nodeid", "", "node id")
	version           = flag.Bool("version", false, "Print the version and exit.")
	concurrentWriters = flag.Int("concurrentWriters", 128, "limit concurrent goroutine writers if not 0")
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

	drv := driver.NewSeaweedFsDriver(*filer, *nodeID, *endpoint)
	drv.ConcurrentWriters = *concurrentWriters
	drv.Run()
}
