//go:build e2e

// Talking to the in-cluster registry from a test.
//
// registryRequest drives one long-lived curl pod with `kubectl exec`: the retention tests poll for
// minutes, and a pod per request would spend that time scheduling. wgetInCluster runs a pod per
// request for the few one-off checks.
package e2e

import (
	"strings"
	"testing"
)

// curlImage is pinned by digest like everything else this project consumes.
const curlImage = "curlimages/curl:8.19.0@sha256:" +
	"c03110c736db81bbe1be0296f1f1608c81b954b01626bdfb0a8f84e5bd00ff3c"

const curlPod = "e2e-curl"

// manifestAccept lists every manifest type containerd asks for, so a correct manifest of any of
// them is served.
const manifestAccept = "application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.oci.image.index.v1+json," +
	"application/vnd.docker.distribution.manifest.v2+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json,*/*"

// ensureCurlPod starts the helper pod once per suite run. It is deliberately left running between
// tests; the cluster's teardown removes it.
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
// response headers (for the status line and Docker-Content-Digest) followed by the body.
//
// A non-2xx is NOT an error: "does this still exist?" is often answered 404, and the negative
// controls depend on asking it.
func registryRequest(t *testing.T, method, path, body, contentType string) string {
	t.Helper()
	ensureCurlPod(t)

	// HEAD uses curl's --head (-I), NOT `-X HEAD`: with -X curl waits for the body that zot's
	// Content-Length announces and never sends, and the exec hangs. -I implies -i.
	method = strings.ToUpper(method)
	verb := []string{"-i", "-X", method}
	if method == "HEAD" {
		verb = []string{"-I"}
	}

	args := []string{"-n", buildNamespace, "exec", curlPod, "--", "curl", "-s"}
	args = append(args, verb...)
	args = append(args, "-H", "Accept: "+manifestAccept)
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

// wgetInCluster runs `wget <flags> <url>` once in a fresh busybox pod and returns its log.
//
// Not `kubectl run --rm -i`, which attaches after the pod starts: a one-request container can exit
// first and the output is lost. Create, wait, then read the log instead. A failed request is
// returned like any other; the caller's assertion reports it better than a timeout would.
func wgetInCluster(t *testing.T, name, url string, flags ...string) string {
	t.Helper()

	args := []string{"-n", buildNamespace, "run", name,
		"--restart=Never", "--image=busybox:1.37", "--command", "--", "wget"}
	args = append(args, flags...)
	args = append(args, "--header", "Accept: "+manifestAccept, url)

	_, _ = kubectl(t, "-n", buildNamespace, "delete", "pod", name, "--ignore-not-found")
	mustKubectl(t, args...)
	t.Cleanup(func() {
		_, _ = kubectl(t, "-n", buildNamespace, "delete", "pod", name, "--ignore-not-found")
	})

	for _, cond := range []string{"Succeeded", "Failed"} {
		if _, err := kubectl(t, "-n", buildNamespace, "wait", "--for=jsonpath={.status.phase}="+cond,
			"pod/"+name, "--timeout=90s"); err == nil {
			break
		}
	}
	out, _ := kubectl(t, "-n", buildNamespace, "logs", name)
	return out
}

// fetchInCluster returns the body at url, fetched from inside the cluster.
func fetchInCluster(t *testing.T, name, url string) string {
	t.Helper()
	return wgetInCluster(t, name, url, "-qO-")
}

// headersInCluster returns only the response headers (a HEAD via `wget -S --spider`).
func headersInCluster(t *testing.T, name, url string) string {
	t.Helper()
	return wgetInCluster(t, name, url, "-S", "--spider")
}
