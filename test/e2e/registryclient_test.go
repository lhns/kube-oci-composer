//go:build e2e

// Talking to the in-cluster registry from a test.
//
// One long-lived pod with curl in it, driven by `kubectl exec`, rather than a pod per request. The
// retention tests poll for over a minute, so a pod per request would mean dozens of pod creations
// and image pulls — minutes of wall clock spent on scheduling rather than on what is being measured.
//
// It also sidesteps the attach race that curlInCluster documents: there is nothing to attach to,
// because the pod is already running and exec returns the command's own output.
package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// curlImage is pinned by digest like everything else this project consumes.
const curlImage = "curlimages/curl:8.19.0@sha256:" +
	"c03110c736db81bbe1be0296f1f1608c81b954b01626bdfb0a8f84e5bd00ff3c"

const curlPod = "e2e-curl"

// ensureCurlPod starts the helper pod once per suite run and leaves it up.
//
// Deliberately NOT cleaned up between tests: the whole point is that it outlives them. up.sh's
// namespace goes with the cluster, which is what removes it.
func ensureCurlPod(t *testing.T) {
	t.Helper()

	if out, err := kubectl(t, "-n", buildNamespace, "get", "pod", curlPod,
		"-o", "jsonpath={.status.phase}"); err == nil && strings.TrimSpace(out) == "Running" {
		return
	}

	_, _ = kubectl(t, "-n", buildNamespace, "delete", "pod", curlPod, "--ignore-not-found")
	mustKubectl(t, "-n", buildNamespace, "run", curlPod,
		"--restart=Never", "--image="+curlImage, "--command", "--", "sleep", "3600")
	mustKubectl(t, "-n", buildNamespace, "wait", "--for=condition=Ready",
		"pod/"+curlPod, "--timeout=120s")
}

// registryRequest performs one HTTP request against the in-cluster registry and returns the
// response headers followed by the body.
//
// Headers are included (`curl -i`) because the assertions need both the status line and
// Docker-Content-Digest, and a registry reports a manifest's identity in a header rather than in
// what it sends back.
//
// A non-2xx is NOT an error here. "Does this manifest still exist?" is a question whose answer is
// often 404, and turning that into a test failure would make the negative controls impossible to
// write.
func registryRequest(t *testing.T, _, method, path, body, contentType string) string {
	t.Helper()
	ensureCurlPod(t)

	args := []string{"-n", buildNamespace, "exec", curlPod, "--",
		"curl", "-s", "-i", "-X", method,
		"-H", "Accept: application/vnd.oci.image.manifest.v1+json," +
			"application/vnd.oci.image.index.v1+json," +
			"application/vnd.docker.distribution.manifest.v2+json," +
			"application/vnd.docker.distribution.manifest.list.v2+json,*/*"}
	if contentType != "" {
		args = append(args, "-H", contentType)
	}
	if body != "" {
		args = append(args, "--data-binary", body)
	}
	args = append(args, "http://"+buildRegistry+path)

	out, _ := kubectl(t, args...)
	return out
}

// shortName builds a readable label for a request. Kept because callers pass one and it makes a
// failing exec traceable to the call that made it.
func shortName(repository, tag string) string {
	repo := repository
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		repo = repo[i+1:]
	}
	return fmt.Sprintf("%s-%s", repo, tag)
}
