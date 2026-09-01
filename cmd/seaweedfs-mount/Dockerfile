# SEAWEEDFS_COMMIT should match the commit in go.mod for reproducibility
# Update via: docker build --build-arg SEAWEEDFS_COMMIT=<hash> ...
ARG SEAWEEDFS_COMMIT=79b87202136c

# Stay on the native build platform and cross-compile from there. Running the
# toolchain under QEMU for every target architecture is slow and makes the git
# clone below fail with "cannot pread pack file: Bad address".
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

RUN apk add git

# Fetch the weed source at a specific commit for reproducibility. Everything
# down to the TARGET* args is architecture independent, so it is downloaded
# once and shared by all target platforms.
ARG SEAWEEDFS_COMMIT
RUN git clone https://github.com/seaweedfs/seaweedfs /go/src/github.com/seaweedfs/seaweedfs \
    && cd /go/src/github.com/seaweedfs/seaweedfs \
    && git checkout ${SEAWEEDFS_COMMIT} \
    && cd weed \
    && go mod download

WORKDIR /go/src/github.com/seaweedfs/seaweedfs-csi-driver
COPY go.mod go.sum ./
RUN go mod download

# Build seaweedfs-mount from current context
COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ENV CGO_ENABLED=0
RUN export GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    && go build -ldflags="-s -w" -o /seaweedfs-mount ./cmd/seaweedfs-mount/main.go \
    && cd /go/src/github.com/seaweedfs/seaweedfs/weed \
    && go build -o /weed .

FROM alpine AS final
RUN apk add fuse
LABEL author="Chris Lu"
COPY --from=builder /weed /usr/bin/
COPY --from=builder /seaweedfs-mount /

RUN chmod +x /seaweedfs-mount
ENTRYPOINT ["/seaweedfs-mount"]
