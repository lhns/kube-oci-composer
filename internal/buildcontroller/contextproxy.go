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

// contextSecretName is the Secret holding one build's context token. A helper because both the
// controller (which mints it) and ContextProxy (which checks it) must agree on it.
func contextSecretName(job string) string { return job + "-context" }

// ContextProxy streams a build's Flux artifact to its own build pod, so build pods never reach
// source-controller, which serves every namespace's artifacts without authentication. It also
// keeps the needed NetworkPolicy inside this chart's namespace. ADR 0044.
//
// A pipe, not a cache; the pod still verifies the digest it was given.
type ContextProxy struct {
	// Client reads the ImageBuild, its Secret and the Flux source. Secrets are uncached, so a
	// freshly minted token is visible immediately.
	Client client.Client
	// HTTP fetches from source-controller, through the same guarded dialer as other fetches.
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
		// Not found and forbidden look the same, so callers cannot enumerate ImageBuilds.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if !p.authorised(ctx, &obj, hash, r.Header.Get("Authorization")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// The URL comes from the object's own sourceRef, never from the request, so the endpoint cannot
	// be pointed elsewhere.
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

	// The Secret's name carries the input hash, so a token opens only the build it was minted for.
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
	// Constant time, so the token cannot be recovered through timing.
	return subtle.ConstantTimeCompare([]byte(presented), want) == 1
}

// stream copies source-controller's response to the caller without buffering: contexts can be
// hundreds of megabytes.
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

// ContextServer is the listener the build pods reach. Separate from the metrics server so a
// NetworkPolicy can admit build namespaces here without admitting them to metrics.
func ContextServer(addr string, proxy *ContextProxy) *http.Server {
	return &http.Server{
		Addr:    addr,
		Handler: proxy.Handler(),
		// Bounds only the header read; bodies may be large.
		ReadHeaderTimeout: 10 * time.Second,
	}
}
