FROM golang:1.25-alpine AS builder

RUN apk add git g++

# Build weed binary
RUN mkdir -p /go/src/github.com/seaweedfs/
RUN git clone https://github.com/seaweedfs/seaweedfs /go/src/github.com/seaweedfs/seaweedfs
RUN cd /go/src/github.com/seaweedfs/seaweedfs/weed && go install && go clean -cache -modcache

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
