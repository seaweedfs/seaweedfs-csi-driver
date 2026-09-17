package driver

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func testStoreFor(t *testing.T, server *httptest.Server) *filerVacStore {
	t.Helper()
	addr := strings.TrimPrefix(server.URL, "http://")
	store, err := newFilerVacStore([]pb.ServerAddress{pb.ServerAddress(addr)})
	if err != nil {
		t.Fatalf("newFilerVacStore: %v", err)
	}
	return store
}

func TestVacStoreSchemeNotInferredFromGrpcCA(t *testing.T) {
	t.Setenv("WEED_GRPC_CA", "/does/not/matter")
	t.Setenv("WEED_VAC_USE_TLS", "")
	s, err := newFilerVacStore([]pb.ServerAddress{"filer:8888"})
	if err != nil {
		t.Fatalf("newFilerVacStore: %v", err)
	}
	if s.scheme != "http" {
		t.Fatalf("grpc.ca alone must keep http scheme, got %q", s.scheme)
	}
	if s.client.Transport != nil {
		t.Fatal("expected no custom TLS transport with vac.use_tls unset")
	}
}

func TestVacStoreExplicitTlsUsesCa(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, caPEM, 0644); err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(server.URL, "https://")

	for _, tc := range []struct {
		name   string
		vacCA  string
		grpcCA string
	}{
		{"vac.ca", caFile, ""},
		{"grpc.ca fallback", "", caFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WEED_VAC_USE_TLS", "true")
			t.Setenv("WEED_VAC_CA", tc.vacCA)
			t.Setenv("WEED_GRPC_CA", tc.grpcCA)
			s, err := newFilerVacStore([]pb.ServerAddress{pb.ServerAddress(addr)})
			if err != nil {
				t.Fatalf("newFilerVacStore: %v", err)
			}
			if s.scheme != "https" {
				t.Fatalf("vac.use_tls=true must use https, got %q", s.scheme)
			}
			if _, err := s.Read(context.Background(), "/buckets/pvc-abc"); err != nil {
				t.Fatalf("request through configured CA: %v", err)
			}
		})
	}
}

func TestVacStoreUnusableCaFails(t *testing.T) {
	badPEM := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(badPEM, []byte("not a pem"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		vacCA string
	}{
		{"missing file", "/does/not/exist"},
		{"malformed pem", badPEM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WEED_VAC_USE_TLS", "true")
			t.Setenv("WEED_VAC_CA", tc.vacCA)
			t.Setenv("WEED_GRPC_CA", "")
			if _, err := newFilerVacStore([]pb.ServerAddress{"filer:8888"}); err == nil {
				t.Fatal("expected configuration error for unusable vac.ca")
			}
		})
	}
}

func TestFilerVacStore_RoundTrip(t *testing.T) {
	server, files := newTestVacServer(t)
	store := testStoreFor(t, server)
	ctx := context.Background()

	params, err := store.Read(ctx, "/buckets/pvc-abc")
	if err != nil || params != nil {
		t.Fatalf("expected nil,nil for unknown volume, got %v, %v", params, err)
	}

	if err := store.Write(ctx, "/buckets/pvc-abc", map[string]string{"diskType": "ssd"}); err != nil {
		t.Fatalf("write: %v", err)
	}
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
	store := testStoreFor(t, server)

	if _, err := store.Read(context.Background(), "/buckets/pvc-abc"); err == nil {
		t.Fatal("expected error for corrupt entry")
	}
}

func TestMergePersistedVolumeAttributes(t *testing.T) {
	context := map[string]string{"diskType": "", "collection": "keep-me", "concurrentReaders": "128"}
	mergePersistedVolumeAttributes(context, map[string]string{
		"diskType":          "ssd",
		"concurrentReaders": "64",
		"collection":        "evil",
		"path":              "/elsewhere",
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
	store, err := newFilerVacStore(nil)
	if err != nil {
		t.Fatalf("newFilerVacStore: %v", err)
	}
	params, err := store.Read(context.Background(), "/buckets/pvc-abc")
	if err != nil || params != nil {
		t.Fatalf("expected nil,nil with no filers, got %v, %v", params, err)
	}
}
