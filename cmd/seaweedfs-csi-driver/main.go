package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/driver"
)

var (
	endpoint = flag.String("endpoint", "unix://tmp/seaweedfs-csi.sock", "CSI endpoint")
	nodeID   = flag.String("nodeid", "", "node id")
	version  = flag.Bool("version", false, "Print the version and exit.")
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

	drv := driver.NewSeaweedFsDriver(*nodeID, *endpoint)
	drv.Run()
}
