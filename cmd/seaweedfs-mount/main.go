package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/seaweedfs/seaweedfs-csi-driver/pkg/mountmanager"
	"github.com/seaweedfs/seaweedfs/weed/glog"
)

var (
	endpoint   = flag.String("endpoint", "unix:///tmp/seaweedfs-mount.sock", "endpoint the mount service listens on")
	weedBinary = flag.String("weedBinary", mountmanager.DefaultWeedBinary, "path to the weed binary")
)

func main() {
	flag.Parse()

	scheme, address, err := mountmanager.ParseEndpoint(*endpoint)
	if err != nil {
		glog.Fatalf("invalid endpoint: %v", err)
	}
	if scheme != "unix" {
		glog.Fatalf("unsupported endpoint scheme: %s", scheme)
	}

	if err := os.Remove(address); err != nil && !errors.Is(err, os.ErrNotExist) {
		glog.Fatalf("removing existing socket: %v", err)
	}

	listener, err := net.Listen("unix", address)
	if err != nil {
		glog.Fatalf("failed to listen on %s: %v", address, err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(address)
	}()

	manager := mountmanager.NewManager(mountmanager.Config{WeedBinary: *weedBinary})

	mux := http.NewServeMux()
	mux.HandleFunc("/mount", makePostHandler(manager.Mount))
	mux.HandleFunc("/unmount", makePostHandler(manager.Unmount))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			glog.Fatalf("server error: %v", err)
		}
	}()

	glog.Infof("mount service listening on %s", *endpoint)

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		glog.Errorf("server shutdown error: %v", err)
	}

	glog.Infof("mount service stopped")
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		glog.Errorf("writing response failed: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, mountmanager.ErrorResponse{Error: message})
}

// makePostHandler creates a generic HTTP POST handler that decodes JSON request,
// calls the manager function, and encodes the JSON response.
func makePostHandler[Req any, Resp any](managerFunc func(*Req) (*Resp, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		var req Req
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
			return
		}

		resp, err := managerFunc(&req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, resp)
	}
}
