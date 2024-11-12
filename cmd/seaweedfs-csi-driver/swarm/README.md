# SeaweedFS CSI-Driver Docker Plugin

This Docker plugin integrates the SeaweedFS CSI-Driver with Docker Swarm, enabling SeaweedFS as a distributed storage solution for containerized applications.

## Features

- Distributed storage for Docker Swarm
- High availability with multiple filer support
- Cache management for improved performance
- Support for volume replication
- Compatible with Docker 25.0+ and Swarm mode

## Prerequisites

- Docker Engine 25.0+
- Docker Swarm initialized
- SeaweedFS cluster with at least one filer

## Installation

### Quick Start

```bash
# Install the plugin
docker plugin install --grant-all-permissions \
    docker.io/mycompany/swarm-csi-seaweedfs:v1.2.7

# Configure filer endpoints
docker plugin set swarm-csi-seaweedfs:v1.2.7 \
    FILER=filer1:8888,filer2:8888

# Enable the plugin
docker plugin enable swarm-csi-seaweedfs:v1.2.7
```

## Configuration

### Environment Variables

| Variable              | Description                                         | Default           | Example                    |
|----------------------|-----------------------------------------------------|-------------------|----------------------------|
| `FILER`              | Filer endpoint(s)                                   | -                 | `filer1:8888,filer2:8888`  |
| `CACHE_SIZE`         | Cache size in MB                                    | `256`             | `512`                      |
| `CACHE_DIR`          | Cache directory path                                | `/tmp/seaweedfs`  | `/mnt/cache`               |
| `C_WRITER`           | Concurrent writer limit                             | `32`              | `64`                       |
| `DATACENTER`         | Data center identifier                              | `DefaultDataCenter`| `dc-east-1`                |
| `UID_MAP`            | Local to filer UID mapping                          | -                 | `1000:1000,1001:1001`      |
| `GID_MAP`            | Local to filer GID mapping                          | -                 | `1000:1000,1001:1001`      |
| `MOUNT_OPTIONS`      | Additional mount options                            | -                 | `allow_other,direct_io`    |
| `LOG_LEVEL`          | Logging verbosity (0-5)                            | `2`               | `3`                        |

### Mount Points

| Destination      | Source           | Purpose                        | Options          |
|-----------------|------------------|--------------------------------|------------------|
| `/node_hostname`| `/etc/hostname`  | Node identification            | `rbind,ro`       |
| `/tmp`          | `/tmp`           | Local caching                  | `rbind,rw`       |
| `/data/published`| -               | Volume mount point             | `rbind,rw`       |

## Usage Examples

### Basic Volume Creation

```bash
docker volume create \
    --driver swarm-csi-seaweedfs:v1.2.7 \
    my_volume
```

### Replicated Volume for Swarm

```bash
docker volume create \
    --driver swarm-csi-seaweedfs:v1.2.7 \
    --scope swarm \
    --availability active \
    shared_volume
```

### Deploy Service with Volume

```bash
docker service create \
    --name myapp \
    --replicas 3 \
    --mount type=volume,source=shared_volume,target=/data,volume-driver=swarm-csi-seaweedfs:v1.2.7 \
    nginx:latest
```

### Stateful Service Example

```bash
docker service create \
    --name database \
    --replicas 1 \
    --mount type=volume,source=db_volume,target=/var/lib/mysql,volume-driver=swarm-csi-seaweedfs:v1.2.7 \
    --env MYSQL_ROOT_PASSWORD=mysecret \
    mysql:8.0
```

## Building from Source

### Prerequisites

- Docker 25.0+
- Git
- Bash

### Build Steps

1. Clone the repository:
```bash
git clone https://github.com/your-repo/swarm-csi-seaweedfs.git
cd swarm-csi-seaweedfs
```

2. Build the plugin:
```bash
# For Docker Hub
./build.sh docker.io/mycompany 3.61 v1.2.7 swarm-csi-seaweedfs linux/amd64

# For local registry
./build.sh localhost:5000/myteam latest v1.0.0 swarm-csi-seaweedfs linux/amd64

# For multi-arch build
./build.sh myregistry.com/storage 3.61 v1.2.7 swarm-csi-seaweedfs linux/arm64
```

## Troubleshooting

### Common Issues

1. **Plugin Installation Fails**
```bash
# Check plugin logs
docker plugin inspect swarm-csi-seaweedfs:v1.2.7 --format '{{.Config.Entrypoint}}'
```

2. **Volume Creation Fails**
```bash
# Verify filer connectivity
curl -v http://filer:8888/dir/status

# Check plugin status
docker plugin ls
```

3. **Mount Issues**
```bash
# Check mount points
mount | grep seaweedfs

# Verify volume status
docker volume inspect my_volume
```

## Storage Location

Volumes are stored in the "/buckets" directory on the SeaweedFS server. Each volume gets its own subdirectory for isolation.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the Apache License 2.0 - see the LICENSE file for details.
