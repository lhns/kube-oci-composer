package serve

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The endpoint accepted writes from anywhere in the cluster for as long as it has existed: the bind
// address defaults to every interface, the chart exposes it as a Service, the Ingress routes /v2/
// which includes PUT, and there is no authentication in this package. Anything able to reach the
// Service could push a manifest or repoint a mutable tag.
//
// Driven through Server.Handler() rather than the helper, because a correct helper that is not
// wired proves nothing.

func TestWritesFromOffLoopbackAreRefused(t *testing.T) {
	srv := emptyServer(t)

	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/v2/team/app/manifests/latest", strings.NewReader("{}"))
			req.RemoteAddr = "10.244.0.7:34567"
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s from a pod address answered %d, want 403: anything in the cluster can "+
					"write to this endpoint", method, rec.Code)
			}

			// The distribution error shape, so a client reports something it understands.
			var body struct {
				Errors []struct{ Code, Message string } `json:"errors"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("refusal is not a registry error document: %v\n%s", err, rec.Body.String())
			}
			if len(body.Errors) == 0 || body.Errors[0].Code != "DENIED" {
				t.Errorf("error code = %+v, want DENIED", body.Errors)
			}
		})
	}
}

// Reads must stay anonymous from anywhere. That is the point of the endpoint — a kubelet pulls
// without credentials — and restricting who may pull is a NetworkPolicy question (threat model I5).
func TestReadsFromOffLoopbackStillWork(t *testing.T) {
	srv, digest := serverWithBlob(t, []byte("layer bytes"))

	req := httptest.NewRequest(http.MethodGet, "/v2/team/app/blobs/"+digest, nil)
	req.RemoteAddr = "10.244.0.7:34567"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous pull from a pod address answered %d, want 200", rec.Code)
	}
}

// The v2 base endpoint is a GET and must stay reachable, or a client concludes the registry does
// not exist before it asks for anything.
func TestTheV2BaseEndpointStillWorks(t *testing.T) {
	srv := emptyServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.RemoteAddr = "10.244.0.7:34567"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /v2/ answered %d, want 200", rec.Code)
	}
}

// The half that would break publishing if the guard were too strict: the controller's own write
// goes over a real TCP connection to 127.0.0.1 and must still be accepted. Driven through a
// listening server, because RemoteAddr is only real when a connection is — httptest.NewServer
// binds loopback, which is exactly the case under test.
func TestTheControllersOwnLoopbackWriteStillWorks(t *testing.T) {
	srv := emptyServer(t)
	backend := httptest.NewServer(srv.Handler())
	defer backend.Close()

	manifest := []byte(`{"schemaVersion":2,` +
		`"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","size":0,` +
		`"digest":"sha256:` + strings.Repeat("0", 64) + `"},"layers":[]}`)

	req, err := http.NewRequest(http.MethodPut,
		backend.URL+"/v2/team/app/manifests/latest", bytes.NewReader(manifest))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")

	resp, err := backend.Client().Do(req)
	if err != nil {
		t.Fatalf("loopback PUT: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("the controller's own loopback write was refused; publishing is broken")
	}
}

// A peer address the guard cannot parse must be refused, not waved through: the failure mode of
// guessing is an open write path.
func TestAnUnparseablePeerIsRefused(t *testing.T) {
	for _, addr := range []string{"", "not-an-address", "127.0.0.1"} {
		if fromLoopback(addr) {
			t.Errorf("fromLoopback(%q) = true; an address without a port is not a peer we know", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:5000", "[::1]:5000"} {
		if !fromLoopback(addr) {
			t.Errorf("fromLoopback(%q) = false; the controller's own writes would be refused", addr)
		}
	}
}

func emptyServer(t *testing.T) *Server {
	t.Helper()
	srv, _ := serverWithBlob(t, []byte("unused"))
	return srv
}
