# SeaweedFS CSI-Driver Docker Plugin

This Docker plugin integrates the SeaweedFS CSI-Driver with Docker. It allows you to use SeaweedFS as a volume driver in Docker environments.

## Environment Variables

| Variable              | Description                                                                                           |
|-----------------------|-------------------------------------------------------------------------------------------------------|
| `FILER`               | Filer endpoint(s), format: `<IP1>:<PORT>,<IP2>:<PORT2>`                                                |
| `CACHE_SIZE`          | The size of the cache to use in MB. Default: 256MB                                                     |
| `CACHE_DIR`           | The cache directory.                                                                                   |
| `C_WRITER`            | Limit concurrent goroutine writers if not 0. Default 32                                                |
| `DATACENTER`          | Data center this node is running in (locality-definition). Default: `DefaultDataCenter`                |
| `UID_MAP`             | Map local UID to UID on filer, comma-separated `<local_uid>:<filer_uid>`                               |
| `GID_MAP`             | Map local GID to GID on filer, comma-separated `<local_gid>:<filer_gid>`                               |

## Mounts

| Mount Destination   | Source Path     | Description                  |
|---------------------|-----------------|------------------------------|
| `/node_hostname`    | `/etc/hostname` | Used to get the nodename     |
| `/tmp`              | `/tmp`          | Used for caching             |

## Usag

```bash
docker plugin install --disable --alias seaweedfs-csi:swarm --grant-all-permissions gradlon/swarm-csi-swaweedfs:v1.2.0
docker plugin set seaweedfs-csi:swarm FILER=<IP>:8888,<IP>:8888
docker plugin set seaweedfs-csi:swarm CACHE_SIZE=512
docker plugin enable seaweedfs-csi:swarm
docker volume create --driver seaweedfs-csi:swarm --availability active --scope single --sharing none  --type mount --opt path="/docker/volumes/teste1" test-volume

docker volume create \
  --driver seaweedfs-csi:swarm \
  --availability active \
  --scope multi \
  --sharing all \
  --type mount \
  testVolume

  docker service create   --name testService   --mount type=cluster,src=testVolume,dst=/usr/share/nginx/html   --publish 2080:80   nginx
```

Volumes are store in the "/buckets" folder on the Seaweed server.
