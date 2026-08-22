package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func tlsRender(t *testing.T, args ...string) string {
	t.Helper()
	return render(t, append([]string{
		"--set", "registry.publish.mode=internalOnly",
		"--set", "registry.tls.enabled=true",
	}, args...)...)
}

// TestTheGeneratedCAIsTheSameCAEverywhere is the most important test in this file, and it guards a
// bug that would otherwise be nearly invisible in review.
//
// `genCA` is not deterministic, and `lookup` returns nothing during `helm template`, during
// `--dry-run`, and on every FIRST INSTALL. So if the TLS Secret and the trust ConfigMap each called
// the material helper, they would each generate their own unrelated CA. The render succeeds. The
// diff looks right. Then `helm install` writes a certificate signed by one CA and a trust bundle
// containing another, and the controllers fail with `x509: certificate signed by unknown authority`
// against a registry the same chart just installed.
//
// It is the password-reuse problem's nastier cousin: that one surfaces as a 401 on UPGRADE, a path
// people watch. This one surfaces only on a fresh install, the path with the least scrutiny.
func TestTheGeneratedCAIsTheSameCAEverywhere(t *testing.T) {
	out := tlsRender(t)

	var secretCA, configMapCA string
	for _, doc := range strings.Split(out, "\n---") {
		var probe struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &probe); err != nil {
			continue
		}
		switch probe.Kind {
		case "Secret":
			if !strings.HasSuffix(probe.Metadata.Name, "-registry-tls") {
				continue
			}
			var s corev1.Secret
			if err := yaml.Unmarshal([]byte(doc), &s); err != nil {
				t.Fatalf("parsing the TLS Secret: %v", err)
			}
			secretCA = s.StringData["ca.crt"]
		case "ConfigMap":
			if !strings.HasSuffix(probe.Metadata.Name, "-registry-ca") {
				continue
			}
			var cm corev1.ConfigMap
			if err := yaml.Unmarshal([]byte(doc), &cm); err != nil {
				t.Fatalf("parsing the CA ConfigMap: %v", err)
			}
			configMapCA = cm.Data["ca.crt"]
		}
	}

	if secretCA == "" {
		t.Fatal("the TLS Secret carries no ca.crt; the controllers would have nothing to trust")
	}
	if configMapCA == "" {
		t.Fatal("no CA ConfigMap rendered; the controllers would have nothing to mount")
	}
	if secretCA != configMapCA {
		t.Fatal("the certificate and the trust bundle come from DIFFERENT CAs.\n" +
			"This renders cleanly and fails on first install with x509: certificate signed by " +
			"unknown authority. The material must be computed once, in one file, into one variable.")
	}
	// And it must be a certificate, not an empty string that happens to match.
	if !strings.Contains(secretCA, "BEGIN CERTIFICATE") {
		t.Fatalf("ca.crt is not a PEM certificate: %q", secretCA)
	}
}

// TestTLSRemovesTheInsecureFlag is the silent-downgrade guard.
//
// The same list becomes BuildKit's `registry.insecure=true`, which means allow plaintext AND skip
// verification. If the Service name stayed on it with TLS on, the controllers would fail loudly —
// but builds would keep pushing the Basic auth header in the clear, and the chart would look like
// it had closed threat I7 while it had not.
func TestTLSRemovesTheInsecureFlag(t *testing.T) {
	if out := tlsRender(t); strings.Contains(out, "--insecure-registry") {
		t.Errorf("with TLS on, nothing may be marked insecure:\n%s", grepFlags(out))
	}
	// The control: with TLS off it must still be there, or the controllers try TLS against a
	// plain-HTTP registry.
	off := render(t, "--set", "registry.publish.mode=internalOnly")
	if !strings.Contains(off, "--insecure-registry") {
		t.Error("without TLS the in-cluster Service must be marked insecure")
	}
}

// TestBothProbesFollowTheListener — zot serves HTTP or HTTPS, never both, so a probe on the wrong
// scheme fails every check and the pod never becomes ready.
func TestBothProbesFollowTheListener(t *testing.T) {
	if got := strings.Count(tlsRender(t), "scheme: HTTPS"); got != 2 {
		t.Errorf("expected both probes on HTTPS, found %d", got)
	}
	if strings.Contains(render(t, "--set", "registry.publish.mode=internalOnly"), "scheme: HTTPS") {
		t.Error("without TLS the probes must not claim HTTPS")
	}
}

// TestTheControllersAreToldWhereTheCAIs — the flag and the mount have to arrive together, in both
// controllers. Either one alone is a controller that cannot verify the registry it was just
// pointed at.
func TestTheControllersAreToldWhereTheCAIs(t *testing.T) {
	out := tlsRender(t)
	for _, deploy := range []string{"t", "builder"} {
		_ = deploy
	}
	if strings.Count(out, "--registry-ca-file=/etc/oci-composer/registry-ca/ca.crt") != 2 {
		t.Errorf("both controllers need the CA flag:\n%s", grepFlags(out))
	}
	if strings.Count(out, "name: registry-ca") < 4 { // one mount + one volume, twice
		t.Error("both controllers need the CA mounted, not just the flag")
	}
}

// TestTrustCanBeTurnedOffForAPubliclyTrustedCertificate. An ACME issuer produces a certificate the
// controller image already trusts, and mounting a CA for it would be noise.
func TestTrustCanBeTurnedOffForAPubliclyTrustedCertificate(t *testing.T) {
	out := tlsRender(t, "--set", "registry.tls.trust.enabled=false")
	if strings.Contains(out, "--registry-ca-file") {
		t.Error("trust.enabled=false must not mount or reference a CA")
	}
	// TLS itself is still on — only the trust distribution is off.
	if !strings.Contains(out, "scheme: HTTPS") {
		t.Error("turning trust off must not turn TLS off")
	}
}

// TestCertManagerModeRendersACertificateAndNoSecret — cert-manager owns the Secret, so the chart
// creating one too would fight it.
func TestCertManagerModeRendersACertificateAndNoSecret(t *testing.T) {
	out := tlsRender(t,
		"--set", "registry.tls.mode=certManager",
		"--set", "registry.tls.certManager.issuerRef.name=my-issuer",
	)
	if !strings.Contains(out, "kind: Certificate") {
		t.Fatal("certManager mode must render a Certificate")
	}
	if strings.Contains(out, "-registry-tls\n") && strings.Contains(out, "type: kubernetes.io/tls") {
		t.Error("certManager mode must not also generate a TLS Secret; cert-manager writes it")
	}
	// The SANs must include the in-cluster names, or the controllers cannot verify the host they
	// actually connect to.
	if !strings.Contains(out, ".svc.cluster.local") {
		t.Errorf("the Certificate must cover the in-cluster Service name:\n%s", out)
	}
}

// TestCertManagerModeNeedsAnIssuer — a Certificate with no issuer is silently never issued, and the
// registry pod waits forever for a Secret nothing writes.
func TestCertManagerModeNeedsAnIssuer(t *testing.T) {
	out := renderExpectingFailure(t,
		"--set", "registry.publish.mode=internalOnly",
		"--set", "registry.tls.enabled=true",
		"--set", "registry.tls.mode=certManager",
	)
	if !strings.Contains(out, "issuerRef") {
		t.Fatalf("the refusal must name the missing issuer:\n%s", out)
	}
}
