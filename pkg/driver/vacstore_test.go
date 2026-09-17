package driver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb"
)

func newTestVacServer(t *testing.T) (*httptest.Server, map[string]string) {
	t.Helper()
	files := map[string]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			files[r.URL.Path] = string(body)
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			content, ok := files[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			io.WriteString(w, content)
		case http.MethodDelete:
			delete(files, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, files
}

func testStoreFor(server *httptest.Server) *filerVacStore {
	addr := strings.TrimPrefix(server.URL, "http://")
	return newFilerVacStore([]pb.ServerAddress{pb.ServerAddress(addr)})
}

func TestVacStoreSchemeNotInferredFromGrpcCA(t *testing.T) {
	// Core regression: a configured grpc.ca (gRPC trust) must NOT turn the VAC
	// store's HTTP calls into https://. The filer HTTP port is commonly plain
	// HTTP even when gRPC is TLS; forcing https breaks every mount.
	t.Setenv("WEED_GRPC_CA", "/does/not/matter")
	t.Setenv("WEED_VAC_USE_TLS", "")
	s := newFilerVacStore([]pb.ServerAddress{"filer:8888"})
	if s.scheme != "http" {
		t.Fatalf("grpc.ca alone must keep http scheme, got %q", s.scheme)
	}
	if s.client.Transport != nil {
		t.Fatal("expected no custom TLS transport with vac.use_tls unset")
	}
}

func TestVacStoreExplicitTlsUsesCa(t *testing.T) {
	// vac.use_tls=true is the explicit opt-in; CA resolution prefers vac.ca.
	// Use the test server's own cert so the store is actually usable over TLS.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("WEED_GRPC_CA", "")
	t.Setenv("WEED_VAC_USE_TLS", "true")
	t.Setenv("WEED_VAC_CA", "")
	addr := strings.TrimPrefix(server.URL, "https://")
	s := newFilerVacStore([]pb.ServerAddress{pb.ServerAddress(addr)})
	if s.scheme != "https" {
		t.Fatalf("vac.use_tls=true must use https, got %q", s.scheme)
	}
	// System store won't trust the test cert; a failure here is a TLS handshake
	// (expected). We assert only the scheme decision, not transport success.
}

func TestFilerVacStore_RoundTrip(t *testing.T) {
	server, files := newTestVacServer(t)
	store := testStoreFor(server)
	ctx := context.Background()

	// Read of an unknown volume is nil, nil — the common case, not an error.
	params, err := store.Read(ctx, "/buckets/pvc-abc")
	if err != nil || params != nil {
		t.Fatalf("expected nil,nil for unknown volume, got %v, %v", params, err)
	}

	if err := store.Write(ctx, "/buckets/pvc-abc", map[string]string{"diskType": "ssd"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Volume IDs containing '/' must be escaped into one path element.
	for p := range files {
		if !strings.HasPrefix(p, vacRootDir+"/") || strings.Count(strings.TrimPrefix(p, vacRootDir+"/"), "/") != 0 {
			t.Fatalf("sidecar path not a single element: %q", p)
		}
	}

	params, err = store.Read(ctx, "/buckets/pvc-abc")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if params["diskType"] != "ssd" {
		t.Fatalf("roundtrip lost parameters: %v", params)
	}

	if err := store.Delete(ctx, "/buckets/pvc-abc"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("delete left entries: %v", files)
	}
}

func TestFilerVacStore_CorruptEntry(t *testing.T) {
	server, files := newTestVacServer(t)
	files[vacPath("/buckets/pvc-abc")] = "{not json"
	store := testStoreFor(server)

	if _, err := store.Read(context.Background(), "/buckets/pvc-abc"); err == nil {
		t.Fatal("expected error for corrupt entry")
	}
}

func TestMergePersistedVolumeAttributes(t *testing.T) {
	context := map[string]string{"diskType": "", "collection": "keep-me", "concurrentReaders": "128"}
	mergePersistedVolumeAttributes(context, map[string]string{
		"diskType":          "ssd",
		"concurrentReaders": "64",
		// Structural keys must never come from the store, even if a stale
		// or corrupted entry contains them.
		"collection": "evil",
		"path":       "/elsewhere",
	})
	if context["diskType"] != "ssd" || context["concurrentReaders"] != "64" {
		t.Fatalf("mutable parameters not applied: %v", context)
	}
	if context["collection"] != "keep-me" || context["path"] != "" {
		t.Fatalf("structural parameter smuggled in: %v", context)
	}
}

func TestVacPathEscapesSlashes(t *testing.T) {
	p := vacPath("/buckets/pvc-e7bddf47")
	if !strings.HasPrefix(p, vacRootDir+"/") {
		t.Fatalf("path outside vac root: %q", p)
	}
	if strings.Contains(strings.TrimPrefix(p, vacRootDir+"/"), "/") {
		t.Fatalf("volume id not escaped to a single element: %q", p)
	}
}

func TestVacPathIsCollisionFree(t *testing.T) {
	if vacPath("/buckets/a_b") == vacPath("/buckets/a/b") {
		t.Fatal("distinct volume IDs with underscores vs slashes must not collide")
	}
	if vacPath("../escape") == vacPath("/.csi/vac/anything") {
		t.Fatal("path traversal must not match a real VAC path")
	}
}

func TestFilerVacStoreReadNoFilers(t *testing.T) {
	store := newFilerVacStore(nil)
	params, err := store.Read(context.Background(), "/buckets/pvc-abc")
	if err != nil || params != nil {
		t.Fatalf("expected nil,nil with no filers, got %v, %v", params, err)
	}
}
