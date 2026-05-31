# The CSI controller implements the volume lifecycle RPCs (CreateVolume,
# DeleteVolume, ExpandVolume, ...). Nomad calls the controller when you run
# `nomad volume create` / `nomad volume delete`. Without a controller plugin
# registered, `nomad volume create` fails with:
#   Error creating volume: Unexpected response code: 500 (rpc error: plugin has no controller)
#
# The controller only talks to the filer (it creates/removes the bucket or
# directory backing the volume), so unlike the node plugin it does NOT need the
# seaweedfs-mount sidecar, host volumes, bidirectional mount propagation, or
# privileged mode. A single instance is enough; run it as a `service` job.
#
# The csi_plugin.id ("seaweedfs") MUST match the id used by the node job
# (seaweedfs-csi.hcl) so Nomad treats them as one logical plugin with both a
# controller and nodes.
job "seaweedfs-csi-controller" {
  datacenters = ["dc1"]
  type        = "service"
  priority    = 90

  group "controller" {
    count = 1

    task "plugin" {
      driver = "docker"

      config {
        image        = "chrislusf/seaweedfs-csi-driver:v1.3.9"
        force_pull   = true
        network_mode = "host"
        args = [
          "--endpoint=unix:///csi-sock/csi.sock",
          "--filer=seaweedfs-filer.service.consul:8888",
          "--components=controller",
        ]
      }

      csi_plugin {
        id        = "seaweedfs"
        type      = "controller"
        mount_dir = "/csi-sock"
      }

      resources {
        cpu        = 200
        memory     = 128
        memory_max = 256
      }
    }
  }
}
