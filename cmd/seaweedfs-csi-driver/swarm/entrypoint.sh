#!/bin/sh

MAX_RETRIES=5
RETRY_INTERVAL=5

check_filer_connection() {
    local filer=$1
    curl -s -f "http://${filer}/dir/status" > /dev/null
    return $?
}

wait_for_filer() {
    local filer=$1
    local retries=0
    
    echo "Checking connection to filer: ${filer}"
    while [ $retries -lt $MAX_RETRIES ]; do
        if check_filer_connection "${filer}"; then
            echo "Successfully connected to filer ${filer}"
            return 0
        fi
        retries=$((retries + 1))
        echo "Failed to connect to filer ${filer}, attempt ${retries}/${MAX_RETRIES}"
        sleep $RETRY_INTERVAL
    done
    
    return 1
}

cleanup_orphaned_mounts() {
    echo "Checking for orphaned mounts..."
    # Nettoyer les points de montage orphelins dans /data/published
    for mount in $(find /data/published -maxdepth 1 -type d); do
        if ! mountpoint -q "$mount"; then
            echo "Cleaning up orphaned mount: $mount"
            rm -rf "$mount"
        fi
    done
}

if [ -z "$FILER" ]; then
    echo "FILER is not set!"
    exit 1
fi

# Validate and split filer endpoints
IFS=',' read -r -a FILER_ENDPOINTS <<< "$FILER"
CONNECTED=false

for endpoint in "${FILER_ENDPOINTS[@]}"; do
    if wait_for_filer "$endpoint"; then
        CONNECTED=true
        ACTIVE_FILER=$endpoint
        break
    fi
done

if [ "$CONNECTED" = false ]; then
    echo "Failed to connect to any filer endpoint after ${MAX_RETRIES} attempts"
    exit 1
fi

NODE_ID=$(cat /node_hostname)
C_WRITER=${C_WRITER:-32}

# Ensure cache directory exists with proper permissions
CACHE_DIR=${CACHE_DIR:-/tmp/seaweedFS/docker-csi}
mkdir -p "$CACHE_DIR"
chmod 755 "$CACHE_DIR"

# Cleanup any orphaned mounts before starting
cleanup_orphaned_mounts

# Set up periodic health check
(
    while true; do
        if ! check_filer_connection "$ACTIVE_FILER"; then
            echo "Lost connection to primary filer, attempting failover..."
            for endpoint in "${FILER_ENDPOINTS[@]}"; do
                if [ "$endpoint" != "$ACTIVE_FILER" ] && wait_for_filer "$endpoint"; then
                    echo "Failing over to $endpoint"
                    ACTIVE_FILER=$endpoint
                    break
                fi
            done
        fi
        sleep 30
    done
) &

# Start the CSI driver with improved options
CMD="/seaweedfs-csi-driver \
    --filer=$FILER \
    --nodeid=${NODE_ID} \
    --endpoint=unix://run/docker/plugins/seaweed.sock \
    --concurrentWriters=${C_WRITER} \
    --dataCenter=${DATACENTER} \
    --dataLocality=none \
    --logtostderr \
    --map.uid=${UID_MAP} \
    --map.gid=${GID_MAP} \
    --cacheCapacityMB=${CACHE_SIZE:-256} \
    --cacheDir=${CACHE_DIR} \
    --v=2"

exec $CMD
