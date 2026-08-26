package buildcontroller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// artifact is a stand-in for source-controller.
func artifact(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// tokenSecret is the Secret the controller mints for one build.
func tokenSecret(namespace, job, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: contextSecretName(job)},
		Data:       map[string][]byte{contextTokenKey: []byte(token)},
	}
}

// buildWithSource is sampleBuild placed in a named namespace, referencing a named Flux source.
func buildWithSource(namespace, name, src string) *ociv1alpha1.ImageBuild {
	obj := sampleBuild()
	obj.Namespace, obj.Name = namespace, name
	obj.Spec.Context.SourceRef.Name = src
	return obj
}

// proxyFor wires a ContextProxy over the given objects.
func proxyFor(t *testing.T, objs ...client.Object) *ContextProxy {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	return &ContextProxy{Client: c, HTTP: http.DefaultClient}
}

// ask makes one request the way a build pod would.
func ask(t *testing.T, p *ContextProxy, namespace, name, hash, token string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/contexts/"+namespace+"/"+name+"/"+hash, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	return rec.Result()
}

// TestTheRightTokenGetsTheContext anchors the negative cases below: without this passing, a
// refusal proves nothing.
func TestTheRightTokenGetsTheContext(t *testing.T) {
	srv := artifact(t, "TARBALL")
	obj := buildWithSource("ns", "app", "src")
	p := proxyFor(t,
		obj,
		gitRepository("ns", "src", srv.URL+"/a.tar.gz", "sha256:aaa"),
		tokenSecret("ns", jobName(obj, testHash), "good-token"),
	)

	resp := ask(t, p, "ns", "app", testHash, "good-token")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "TARBALL" {
		t.Errorf("body = %q", body)
	}
}

// TestOneBuildsTokenCannotFetchAnothers is the guard this endpoint exists for.
//
// The whole point of proxying is that a build reaches its OWN source and nothing else. If a token
// minted for one build opened another, this would be exactly the unauthenticated read it replaced,
// with an extra hop.
func TestOneBuildsTokenCannotFetchAnothers(t *testing.T) {
	srv := artifact(t, "MINE")
	mine := buildWithSource("ns", "mine", "src")
	theirs := buildWithSource("ns", "theirs", "src")

	p := proxyFor(t,
		mine, theirs,
		gitRepository("ns", "src", srv.URL+"/a.tar.gz", "sha256:aaa"),
		tokenSecret("ns", jobName(mine, testHash), "mine-token"),
		tokenSecret("ns", jobName(theirs, testHash), "theirs-token"),
	)

	resp := ask(t, p, "ns", "theirs", testHash, "mine-token")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("one build's token fetched another's context: status = %d, want 403", resp.StatusCode)
	}
}

// TestAnUnauthenticatedRequestIsRefused — the endpoint is reachable from every namespace that owns
// an ImageBuild, so "reachable" must not mean "readable".
func TestAnUnauthenticatedRequestIsRefused(t *testing.T) {
	srv := artifact(t, "TARBALL")
	obj := buildWithSource("ns", "app", "src")
	p := proxyFor(t, obj,
		gitRepository("ns", "src", srv.URL+"/a.tar.gz", "sha256:aaa"),
		tokenSecret("ns", jobName(obj, testHash), "good-token"))

	for _, tc := range []struct{ name, token string }{
		{"no token", ""},
		{"wrong token", "guessed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if resp := ask(t, p, "ns", "app", testHash, tc.token); resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

// TestAStaleHashIsRefused — the Secret's name carries the input hash, so a token from a previous
// build of the same object names a Secret that no longer exists.
func TestAStaleHashIsRefused(t *testing.T) {
	srv := artifact(t, "TARBALL")
	obj := buildWithSource("ns", "app", "src")
	p := proxyFor(t, obj,
		gitRepository("ns", "src", srv.URL+"/a.tar.gz", "sha256:aaa"),
		tokenSecret("ns", jobName(obj, testHash), "good-token"))

	if resp := ask(t, p, "ns", "app", "0000000000000000", "good-token"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a token for a different input hash was accepted: status = %d, want 403", resp.StatusCode)
	}
}

// TestAnUnknownBuildIs404 — and says nothing more, because enumerating which ImageBuilds exist is
// itself a small leak and it is free not to.
func TestAnUnknownBuildIs404(t *testing.T) {
	p := proxyFor(t)
	if resp := ask(t, p, "ns", "nope", testHash, "any"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestASourceRefBuildNeverNamesSourceController is the other half of the guarantee: the endpoint is
// pointless if the Job still hands the pod a flux-system URL.
func TestASourceRefBuildNeverNamesSourceController(t *testing.T) {
	cfg := sampleConfig()
	cfg.ContextBaseURL = "http://oci-builder-context.oci-composer.svc:8090"
	obj := buildWithSource("team-a", "app", "src")

	job := buildJob(obj, testHash, "http://source-controller.flux-system.svc/gitrepository/a/b/c.tar.gz",
		"sha256:ctx", cfg, sampleRepo, "", "", "", "app-ctx-secret", true)

	args := strings.Join(job.Spec.Template.Spec.InitContainers[0].Args, " ")
	if strings.Contains(args, "flux-system") {
		t.Errorf("the fetcher is still pointed at source-controller: %s", args)
	}
	want := "--url=http://oci-builder-context.oci-composer.svc:8090/contexts/team-a/app/" + testHash
	if !strings.Contains(args, want) {
		t.Errorf("argv does not carry the context endpoint URL.\n got: %s\nwant: %s", args, want)
	}
	if !strings.Contains(args, "--token-file=/context-token/token") {
		t.Errorf("argv does not carry the token file: %s", args)
	}
}

// TestOnlyTheFetcherHoldsTheToken — the build container runs the user's Dockerfile, and a
// credential to the context endpoint in its filesystem would hand every RUN line the reach this
// arrangement exists to remove.
func TestOnlyTheFetcherHoldsTheToken(t *testing.T) {
	cfg := sampleConfig()
	cfg.ContextBaseURL = "http://ctx:8090"
	job := buildJob(buildWithSource("team-a", "app", "src"), testHash, "http://sc/a.tar.gz",
		"sha256:ctx", cfg, sampleRepo, "", "", "", "app-ctx-secret", true)

	pod := job.Spec.Template.Spec
	for _, m := range pod.Containers[0].VolumeMounts {
		if m.Name == contextTokenVolume {
			t.Error("the build container mounts the context token")
		}
	}
	var mounted bool
	for _, m := range pod.InitContainers[0].VolumeMounts {
		if m.Name == contextTokenVolume {
			mounted = true
			if m.SubPath != contextTokenFile || !m.ReadOnly {
				t.Errorf("token mount = %+v; want subPath %q and read-only", m, contextTokenFile)
			}
		}
	}
	if !mounted {
		t.Error("the fetcher does not mount the context token")
	}
}

// TestFetchAndImageContextsStayDirect is the negative control for the scoping decision. Proxying an
// arbitrary user URL would make the controller an SSRF amplifier, which is what internal/netguard
// exists to prevent; those kinds are external by nature and the pod fetches them itself.
func TestFetchAndImageContextsStayDirect(t *testing.T) {
	cfg := sampleConfig()
	cfg.ContextBaseURL = "http://ctx:8090"

	for _, tc := range []struct {
		name string
		ctxt *ociv1alpha1.BuildContext
		url  string
	}{
		{"fetch", &ociv1alpha1.BuildContext{Fetch: &ociv1alpha1.FetchSource{
			URL: "https://example.com/src.tgz", Digest: "sha256:aaa", Unpack: "tar.gz"}},
			"https://example.com/src.tgz"},
		{"image", &ociv1alpha1.BuildContext{Image: &ociv1alpha1.ImageSource{
			Ref: "ghcr.io/me/ctx@sha256:bbb"}}, "ghcr.io/me/ctx@sha256:bbb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := sampleBuild()
			obj.Spec.Context = tc.ctxt
			job := buildJob(obj, testHash, tc.url, "sha256:ctx", cfg, sampleRepo, "", "", "", "", true)

			args := strings.Join(job.Spec.Template.Spec.InitContainers[0].Args, " ")
			if !strings.Contains(args, "--url="+tc.url) {
				t.Errorf("a %s context was rewritten through the controller: %s", tc.name, args)
			}
			if strings.Contains(args, "--token-file") {
				t.Errorf("a %s context was given a context token: %s", tc.name, args)
			}
		})
	}
}
