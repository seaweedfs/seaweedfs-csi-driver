package mountmanager

import (
	"fmt"
	"path/filepath"

	"github.com/seaweedfs/seaweedfs/weed/util"
)

// DefaultSocketDir is the default directory for volume sockets.
const DefaultSocketDir = "/var/lib/seaweedfs-mount"

// LocalSocketPath returns the unix socket path used to communicate with the weed mount process.
// The baseDir parameter should be the directory where sockets are stored (e.g., derived from mountEndpoint).
func LocalSocketPath(baseDir, volumeID string) string {
	if baseDir == "" {
		baseDir = DefaultSocketDir
	}
	hash := util.HashToInt32([]byte(volumeID))
	if hash < 0 {
		hash = -hash
	}
	return filepath.Join(baseDir, fmt.Sprintf("seaweedfs-mount-%d.sock", hash))
}
