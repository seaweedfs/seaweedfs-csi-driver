# id - Nomad internal ID. It is not sent to the CSI plugin but is used by Nomad for `per_alloc`
# volume configurations, etc.
id        = "example-seaweedfs-volume"
# name - the name sent to the CSI plugin as an idempotency key and suggested volume ID. The CSI
# spec requires the calling Container Orchestrator to respect the actual volumeId returned by the
# CSI plugin. Nomad does this, storing it as the volume's ExternalID and using it in subsequent
# calls to the controller and node.
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
  # By default, collection is the `volume ID` returned from the Create Volume gRPC call. Nomad calls this the
  # External ID of the volume. "example" here overrides that.
  collection = "example"
  replication = "000"
  # By default, path is "/buckets/<volume ID>", where `volume ID` is the value returned from the Create Volume gRPC
  # call that Nomad calls the External ID. Do not use relative paths (paths that start with something other than /)
  # They will will not work properly.
  # When `path` is outside of the default path - `/buckets/<volume ID>` - the default path bucket will still be
  # created, but remain empty. Since capabilities checks are tied to the default path of the volume, they may not
  # provide the expected results.
  path = "/buckets/example"
}
