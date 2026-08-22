package reconciler

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCA writes the httptest server's own certificate as a PEM file, which is what an operator
// would mount from the chart.
func writeCA(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	cert := srv.Certificate()
	path := filepath.Join(t.TempDir(), "ca.crt")
	body := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing the CA: %v", err)
	}
	return path
}

// TestTheTransportTrustsTheSuppliedCA — the whole point. A registry serving a certificate signed by
// a CA nothing knows must become reachable, and must not be reachable without it.
func TestTheTransportTrustsTheSuppliedCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The control. Without the CA this must fail, or the test proves nothing about the CA.
	if _, err := (&http.Client{}).Get(srv.URL); err == nil { //nolint:noctx,bodyclose // the request must fail
		t.Fatal("the default transport accepted an untrusted certificate; this test cannot prove anything")
	}

	rt, err := Transport(writeCA(t, srv))
	if err != nil {
		t.Fatalf("building the transport: %v", err)
	}
	resp, err := (&http.Client{Transport: rt}).Get(srv.URL) //nolint:noctx // no context needed here
	if err != nil {
		t.Fatalf("the supplied CA was not trusted: %v", err)
	}
	resp.Body.Close()
}

// TestTheTransportStillVerifies is the guard against the two ways to make this "work" by weakening
// it: starting from an empty pool, or turning verification off.
//
// An empty base pool would serve the operator's own registry perfectly and break every pull from
// ghcr.io, Docker Hub and any registry an object named for itself — and only for the installs that
// set the flag, which is to say only in production.
//
// Note what is NOT asserted here: that the system roots are present. `x509.CertPool.Subjects` is
// deprecated precisely because it does not report system roots, and proving a public certificate
// verifies would need the network. What the code does instead is start from `x509.SystemCertPool`
// and fail startup if that errors, rather than falling back to an empty pool — see Transport.
func TestTheTransportStillVerifies(t *testing.T) {
	trusted := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer trusted.Close()

	// A second server with a certificate this test generates itself.
	//
	// NOT a second httptest.NewTLSServer: those all present the SAME built-in certificate, so
	// trusting one trusts them all and the negative control below would pass for the wrong reason.
	// That is exactly what happened when this test was first written.
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	other.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	other.StartTLS()
	defer other.Close()

	rt, err := Transport(writeCA(t, trusted))
	if err != nil {
		t.Fatalf("building the transport: %v", err)
	}
	client := &http.Client{Transport: rt}

	resp, err := client.Get(trusted.URL) //nolint:noctx // no context needed here
	if err != nil {
		t.Fatalf("the supplied CA must be trusted: %v", err)
	}
	resp.Body.Close()

	// The one that matters: adding a root must not have disabled verification for everything else.
	if _, err := client.Get(other.URL); err == nil { //nolint:noctx,bodyclose // the request must fail
		t.Fatal("a certificate from an unrelated CA was accepted; verification is off, not extended")
	}

	tr, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("expected an *http.Transport, got %T", rt)
	}
	if tr.TLSClientConfig.RootCAs == nil {
		t.Error("no root pool was set at all")
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("verification must never be skipped here")
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Error("TLS 1.2 should be the floor")
	}
}

// TestTheTransportRefusesJunk — a mounted file that is not a certificate must fail at startup, not
// at the first artifact. A controller that cannot trust its registry has nothing useful to do.
func TestTheTransportRefusesJunk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, []byte("this is not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Transport(path)
	if err == nil {
		t.Fatal("a file with no certificate in it must be an error")
	}
	if !strings.Contains(err.Error(), "no certificate") {
		t.Errorf("the error should say what is wrong with the file: %v", err)
	}

	if _, err := Transport(filepath.Join(t.TempDir(), "absent.crt")); err == nil {
		t.Fatal("a missing CA file must be an error")
	}
}

// selfSigned returns a certificate signed by a CA that exists only inside this call, so nothing
// else in the process can be persuaded to trust it.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unrelated.test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"unrelated.test"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating a certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
