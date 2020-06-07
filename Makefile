.PHONY: build container clean

REGISTRY_NAME=seaweedfs
IMAGE_NAME=csi
VERSION ?= dev
IMAGE_TAG=$(REGISTRY_NAME)/$(IMAGE_NAME):$(VERSION)
COMMIT ?= $(shell git rev-parse --short HEAD)
LDFLAGS ?= -X github.com/seaweedfs/seaweedfs-csi-driver/pkg/driver.gitCommit=${COMMIT}

build:
	CGO_ENABLED=0 GOOS=linux go build -a -ldflags '$(LDFLAGS)' -o _output/seaweedfs-csi-driver ./cmd/seaweedfs-csi-driver/main.go
container: build
	docker build -t $(IMAGE_TAG) -f cmd/seaweedfs-csi-driver/Dockerfile.dev .
push: container
	docker push $(IMAGE_TAG)
clean:
	go clean -r -x
	-rm -rf _output
