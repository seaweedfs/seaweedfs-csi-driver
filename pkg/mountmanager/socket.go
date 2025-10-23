package mountmanager

import (
	"fmt"

	"github.com/seaweedfs/seaweedfs/weed/util"
)

// LocalSocketPath returns the unix socket path used to communicate with the weed mount process.
func LocalSocketPath(volumeID string) string {
	hash := util.HashToInt32([]byte(volumeID))
	if hash < 0 {
		hash = -hash
	}
	return fmt.Sprintf("/var/lib/seaweedfs-mount/seaweedfs-mount-%d.sock", hash)
}
