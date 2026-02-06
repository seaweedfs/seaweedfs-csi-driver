# SEAWEEDFS_COMMIT should match the commit in go.mod for reproducibility
# Update via: docker build --build-arg SEAWEEDFS_COMMIT=<hash> ...
ARG SEAWEEDFS_COMMIT=2163570d1611

FROM golang:1.25-alpine AS builder

RUN apk add git g++

# Build weed binary from a specific commit for reproducibility
ARG SEAWEEDFS_COMMIT
RUN git clone https://github.com/seaweedfs/seaweedfs /go/src/github.com/seaweedfs/seaweedfs \
    && cd /go/src/github.com/seaweedfs/seaweedfs \
    && git checkout ${SEAWEEDFS_COMMIT} \
    && cd weed \
    && go install \
    && go clean -cache -modcache

# Build seaweedfs-mount from current context
WORKDIR /go/src/github.com/seaweedfs/seaweedfs-csi-driver
COPY . .

RUN go build -ldflags="-s -w" -o /seaweedfs-mount ./cmd/seaweedfs-mount/main.go && go clean -cache -modcache

FROM alpine AS final
RUN apk add fuse
LABEL author="Chris Lu"
COPY --from=builder /go/bin/weed /usr/bin/
COPY --from=builder /seaweedfs-mount /

RUN chmod +x /seaweedfs-mount
ENTRYPOINT ["/seaweedfs-mount"]
