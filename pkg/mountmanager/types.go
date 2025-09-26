package mountmanager

// MountRequest contains all information needed to start a weed mount process.
type MountRequest struct {
	VolumeID          string            `json:"volumeId"`
	TargetPath        string            `json:"targetPath"`
	ReadOnly          bool              `json:"readOnly"`
	Filers            []string          `json:"filers"`
	CacheDir          string            `json:"cacheDir"`
	CacheCapacityMB   int               `json:"cacheCapacityMB"`
	ConcurrentWriters int               `json:"concurrentWriters"`
	UidMap            string            `json:"uidMap"`
	GidMap            string            `json:"gidMap"`
	DataCenter        string            `json:"dataCenter"`
	DataLocality      string            `json:"dataLocality"`
	VolumeContext     map[string]string `json:"volumeContext"`
}

// MountResponse is returned after a successful mount request.
type MountResponse struct {
	LocalSocket string `json:"localSocket"`
}

// UnmountRequest contains the information needed to stop a weed mount process.
type UnmountRequest struct {
	VolumeID string `json:"volumeId"`
}

// UnmountResponse is the response of a successful unmount request.
type UnmountResponse struct{}

// ConfigureRequest adjusts the behaviour of an existing mount.
type ConfigureRequest struct {
	VolumeID           string `json:"volumeId"`
	CollectionCapacity int64  `json:"collectionCapacity"`
}

// ConfigureResponse represents a successful configure call.
type ConfigureResponse struct{}

// ErrorResponse is returned when the mount service encounters a failure.
type ErrorResponse struct {
	Error string `json:"error"`
}

const (
	// DefaultWeedBinary is the default executable name used to spawn weed mount processes.
	DefaultWeedBinary = "weed"
)
