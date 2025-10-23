package mountmanager

// MountRequest contains all information needed to start a weed mount process.
type MountRequest struct {
	VolumeID    string   `json:"volumeId"`
	TargetPath  string   `json:"targetPath"`
	CacheDir    string   `json:"cacheDir"`
	MountArgs   []string `json:"mountArgs"`
	LocalSocket string   `json:"localSocket"`
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

// ErrorResponse is returned when the mount service encounters a failure.
type ErrorResponse struct {
	Error string `json:"error"`
}

const (
	// DefaultWeedBinary is the default executable name used to spawn weed mount processes.
	DefaultWeedBinary = "weed"
)
