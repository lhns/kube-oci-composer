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

// writeCA writes the httptest server's certificate as a PEM file, as the chart would mount it.
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

// TestTheTransportTrustsTheSuppliedCA — a registry signed by an unknown CA becomes reachable with
// the CA, and only with it.
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

// TestTheTransportStillVerifies guards against "working" by weakening: verification turned off, or
// an empty base pool. System roots are not asserted directly (CertPool.Subjects omits them and a
// public cert needs the network); Transport fails rather than fall back to an empty pool.
func TestTheTransportStillVerifies(t *testing.T) {
	trusted := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer trusted.Close()

	// Not a second httptest.NewTLSServer: those all share one built-in certificate, so the negative
	// control below would pass for the wrong reason.
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

	// Adding a root must not have disabled verification for everything else.
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

// TestTheTransportRefusesJunk — a bad CA file must fail at startup, not at the first artifact.
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

// selfSigned returns a certificate from a CA that exists only inside this call.
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
