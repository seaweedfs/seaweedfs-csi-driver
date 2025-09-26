FROM golang:1.23-alpine AS builder

RUN apk add git g++

RUN mkdir -p /go/src/github.com/seaweedfs/
RUN git clone https://github.com/seaweedfs/seaweedfs /go/src/github.com/seaweedfs/seaweedfs
RUN cd /go/src/github.com/seaweedfs/seaweedfs/weed && go install

RUN mkdir -p /go/src/github.com/zemul/
RUN git clone https://github.com/zemul/seaweedfs-csi-driver /go/src/github.com/zemul/seaweedfs-csi-driver
RUN cd /go/src/github.com/zemul/seaweedfs-csi-driver && \
    go build -ldflags="-s -w" -o /seaweedfs-mount ./cmd/seaweedfs-mount/main.go

FROM alpine AS final
RUN apk add  fuse
COPY --from=builder /go/bin/weed /usr/bin/
COPY --from=builder /seaweedfs-mount /

RUN chmod +x /seaweedfs-mount
ENTRYPOINT ["/seaweedfs-mount"]