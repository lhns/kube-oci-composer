package buildcontroller

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/source"
)

// contextTokenKey is the key inside a build's context Secret.
const contextTokenKey = "token"

// contextSecretName is the Secret holding one build's context token.
//
// A helper where the push, CA and Dockerfile secrets spell their names inline, because this is the
// only one computed in two independent places: the controller mints it, and the endpoint below
// looks it up. A name that must agree across a trust boundary gets one definition.
func contextSecretName(job string) string { return job + "-context" }

// ContextProxy streams a build's Flux artifact to its own build pod.
//
// It exists so the build pod never talks to source-controller. source-controller serves artifacts
// over plain HTTP with NO authentication, at /gitrepository/<ns>/<name>/<sha>.tar.gz -- so a build
// pod able to reach it can fetch ANY namespace's source, not merely its own. Every build pod having
// that reach is a cross-tenant read primitive, and the NetworkPolicy people write to make builds
// work on a default-deny cluster is what grants it.
//
// Pointing the pod here instead also makes the connectivity problem solvable. The registry's policy
// works because the registry is in the release namespace; an equivalent for the fetch leg would
// need an ingress rule in flux-system, which this chart does not own. The builder it does.
//
// A pipe, not a cache: nothing is stored, and the pod still verifies the digest it was given, so a
// wrong or compromised answer from here is caught rather than built.
type ContextProxy struct {
	// Client reads the ImageBuild, its Secret and the Flux source. Secret reads bypass the cache
	// (the manager disables it for Secrets), so a freshly created token is visible immediately.
	Client client.Client
	// HTTP fetches from source-controller. Carries the same guarded dialer as the controller's
	// other fetches, so this cannot be turned into a proxy to somewhere it should not reach.
	HTTP *http.Client
}

// Handler routes the one request this serves.
func (p *ContextProxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /contexts/{namespace}/{name}/{hash}", p.serve)
	return mux
}

// serve answers one build pod asking for its own context.
func (p *ContextProxy) serve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ns, name, hash := r.PathValue("namespace"), r.PathValue("name"), r.PathValue("hash")

	var obj ociv1alpha1.ImageBuild
	if err := p.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &obj); err != nil {
		// Not found and forbidden answer the same way. Telling an unauthenticated caller which
		// ImageBuilds exist is itself a small leak, and it is free not to.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if !p.authorised(ctx, &obj, hash, r.Header.Get("Authorization")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Resolved from the object, never from the request or the Secret. The only URL this will ever
	// fetch is one source-controller published for a source THIS build references -- so a tenant
	// cannot use the endpoint to make the controller fetch something else.
	ref := obj.Spec.Context.GetSourceRef()
	if ref == nil {
		http.Error(w, "this build has no sourceRef context", http.StatusBadRequest)
		return
	}
	if ref.Namespace != "" && ref.Namespace != obj.Namespace {
		http.Error(w, "cross-namespace source", http.StatusForbidden)
		return
	}
	art, err := source.FluxSource(ctx, p.Client, ref.Kind, obj.Namespace, ref.Name)
	if err != nil {
		http.Error(w, "the source is not ready", http.StatusServiceUnavailable)
		return
	}

	if err := p.stream(ctx, w, art.URL); err != nil {
		log.FromContext(ctx).Error(err, "streaming the build context",
			"imagebuild", ns+"/"+name)
	}
}

// authorised compares the presented bearer token with the one this build was issued.
func (p *ContextProxy) authorised(ctx context.Context, obj *ociv1alpha1.ImageBuild,
	hash, header string) bool {

	const prefix = "Bearer "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	presented := header[len(prefix):]

	// The Secret's name carries the input hash, so a token only opens the build it was minted for.
	// A stale token from a previous build of the same object names a Secret that no longer exists.
	var secret corev1.Secret
	key := types.NamespacedName{
		Namespace: obj.Namespace,
		Name:      contextSecretName(jobName(obj, hash)),
	}
	if err := p.Client.Get(ctx, key, &secret); err != nil {
		if !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Error(err, "reading a context token")
		}
		return false
	}
	want := secret.Data[contextTokenKey]
	if len(want) == 0 {
		return false
	}
	// Constant time: the comparison is against a secret, and a timing oracle here would let a
	// caller recover a token byte by byte.
	return subtle.ConstantTimeCompare([]byte(presented), want) == 1
}

// stream copies source-controller's response to the caller.
//
// Streamed rather than buffered. A build context is routinely hundreds of megabytes, and buffering
// would make one build's size the controller's memory ceiling for every other build at once.
func (p *ContextProxy) stream(ctx context.Context, w http.ResponseWriter, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		http.Error(w, "bad artifact URL", http.StatusInternalServerError)
		return err
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		http.Error(w, "the artifact is unavailable", http.StatusBadGateway)
		return fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "the artifact is unavailable", http.StatusBadGateway)
		return fmt.Errorf("fetching %s: %s", url, resp.Status)
	}

	w.Header().Set("Content-Type", "application/gzip")
	if n := resp.Header.Get("Content-Length"); n != "" {
		w.Header().Set("Content-Length", n)
	}
	w.WriteHeader(http.StatusOK)
	_, err = io.Copy(w, resp.Body)
	return err
}

// ContextServer is the listener the build pods reach.
//
// Its own listener rather than a route on the metrics server: metrics are scraped from the release
// namespace, this is reached from every namespace that owns an ImageBuild, and the NetworkPolicy
// that admits the second must not admit the first.
func ContextServer(addr string, proxy *ContextProxy) *http.Server {
	return &http.Server{
		Addr:    addr,
		Handler: proxy.Handler(),
		// A build context can be large and a build namespace can be far away; the read side is a
		// header and nothing more, so it stays short.
		ReadHeaderTimeout: 10 * time.Second,
	}
}
