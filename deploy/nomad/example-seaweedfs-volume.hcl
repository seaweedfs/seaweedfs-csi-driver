id        = "example-seaweedfs-volume"
name      = "example-seaweedfs-volume"
type      = "csi"
plugin_id = "seaweedfs"

capacity_min = "256GiB"
capacity_max = "512GiB"

capability {
  access_mode     = "multi-node-multi-writer"
  attachment_mode = "file-system"
}

# Optional: for 'nomad volume create', specify mount options to validate for
# 'attachment_mode = "file-system". Registering an existing volume will record
# but ignore these fields.
mount_options {
  mount_flags = ["rw"]
}

parameters {
  # Available options: https://github.com/seaweedfs/seaweedfs-csi-driver/blob/master/pkg/driver/mounter_seaweedfs.go
  collection = "example"
  replication = "000"
  path = "/buckets/example"
}
