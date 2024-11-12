#!/bin/bash
set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Default values
VERSION=${2:-latest}
ARCH=${5:-linux/amd64}
PLUGIN_NAME=${4:-swarm-csi-seaweedfs}
PLUGIN_TAG=${3:-v1.2.7}
PREFIX=${1:-local}

# Help function
show_help() {
    echo "Build script for SeaweedFS CSI Docker Plugin"
    echo
    echo "Usage: $0 [PREFIX] [VERSION] [TAG] [PLUGIN_NAME] [ARCH]"
    echo
    echo "Parameters:"
    echo "  PREFIX       Docker registry prefix (e.g., 'docker.io/mycompany')"
    echo "  VERSION      SeaweedFS CSI driver version (e.g., '3.61')"
    echo "  TAG         Plugin version tag (e.g., 'v1.2.7')"
    echo "  PLUGIN_NAME Plugin name (e.g., 'swarm-csi-seaweedfs')"
    echo "  ARCH        Target architecture (e.g., 'linux/amd64', 'linux/arm64')"
    echo
    echo "Examples:"
    echo "  # Build for Docker Hub:"
    echo "  $0 docker.io/mycompany 3.61 v1.2.7 swarm-csi-seaweedfs linux/amd64"
    echo
    echo "  # Build for local registry:"
    echo "  $0 localhost:5000/myteam latest v1.0.0 swarm-csi-seaweedfs linux/amd64"
    echo
    echo "  # Build for multiple architectures:"
    echo "  $0 myregistry.com/storage 3.61 v1.2.7 swarm-csi-seaweedfs linux/arm64"
}

# Error handling function
handle_error() {
    echo -e "${RED}Error: $1${NC}"
    exit 1
}

# Cleanup function
cleanup() {
    echo -e "${YELLOW}Cleaning up temporary resources...${NC}"
    docker container rm -f seaweed-csi_tmp 2>/dev/null || true
    docker image rm -f seaweed-csi_tmp_img 2>/dev/null || true
    rm -rf ./plugin 2>/dev/null || true
}

# Parse command line arguments
if [[ "$1" == "-h" || "$1" == "--help" ]]; then
    show_help
    exit 0
fi

# Trap for cleanup
trap cleanup EXIT

# Log build configuration
echo -e "${GREEN}Building SeaweedFS CSI Plugin with:${NC}"
echo -e "PREFIX:      ${YELLOW}$PREFIX${NC}"
echo -e "VERSION:     ${YELLOW}$VERSION${NC}"
echo -e "TAG:         ${YELLOW}$PLUGIN_TAG${NC}"
echo -e "PLUGIN_NAME: ${YELLOW}$PLUGIN_NAME${NC}"
echo -e "ARCH:        ${YELLOW}$ARCH${NC}"

# Check required files
for file in "config.json" "Dockerfile" "entrypoint.sh"; do
    if [ ! -f "$file" ]; then
        handle_error "Required file $file not found"
    fi
done

# Create plugin directory
echo -e "${GREEN}Creating plugin directory structure...${NC}"
mkdir -p ./plugin/rootfs || handle_error "Failed to create plugin directory"

# Copy config file
echo -e "${GREEN}Copying config.json...${NC}"
cp config.json ./plugin/ || handle_error "Failed to copy config.json"

# Build the image
echo -e "${GREEN}Building Docker image...${NC}"
docker build \
    --platform ${ARCH} \
    --build-arg BASE_IMAGE=chrislusf/seaweedfs-csi-driver:${VERSION} \
    --build-arg ARCH=$ARCH \
    -t seaweed-csi_tmp_img . || handle_error "Docker build failed"

# Create and export container
echo -e "${GREEN}Creating temporary container and extracting rootfs...${NC}"
docker container create --name seaweed-csi_tmp seaweed-csi_tmp_img || handle_error "Failed to create temporary container"
docker container export seaweed-csi_tmp | tar -x -C ./plugin/rootfs || handle_error "Failed to export container filesystem"

# Remove existing plugin if it exists
echo -e "${GREEN}Removing existing plugin if present...${NC}"
docker plugin disable ${PREFIX}/${PLUGIN_NAME}:${PLUGIN_TAG} 2>/dev/null || true
docker plugin rm ${PREFIX}/${PLUGIN_NAME}:${PLUGIN_TAG} 2>/dev/null || true

# Create and push plugin
echo -e "${GREEN}Creating plugin...${NC}"
docker plugin create ${PREFIX}/${PLUGIN_NAME}:${PLUGIN_TAG} ./plugin || handle_error "Failed to create plugin"

echo -e "${GREEN}Pushing plugin to registry...${NC}"
docker plugin push ${PREFIX}/${PLUGIN_NAME}:${PLUGIN_TAG} || handle_error "Failed to push plugin"

echo -e "${GREEN}Build completed successfully!${NC}"
echo -e "${GREEN}Plugin: ${YELLOW}${PREFIX}/${PLUGIN_NAME}:${PLUGIN_TAG}${NC}"

# Usage instructions
echo -e "\n${GREEN}To use the plugin:${NC}"
echo -e "1. Install the plugin:"
echo -e "${YELLOW}docker plugin install --grant-all-permissions ${PREFIX}/${PLUGIN_NAME}:${PLUGIN_TAG}${NC}"
echo -e "\n2. Configure the plugin:"
echo -e "${YELLOW}docker plugin set ${PLUGIN_NAME}:${PLUGIN_TAG} FILER=<IP>:<PORT>${NC}"
echo -e "\n3. Enable the plugin:"
echo -e "${YELLOW}docker plugin enable ${PLUGIN_NAME}:${PLUGIN_TAG}${NC}"
