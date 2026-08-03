package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// dockerConfig is the subset of ~/.docker/config.json this controller understands.
type dockerConfig struct {
	Auths map[string]dockerAuth `json:"auths"`
}

type dockerAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

// secretKeychain resolves credentials from a parsed dockerconfigjson.
type secretKeychain struct {
	auths map[string]authn.AuthConfig
}

// Resolve matches the resource's registry against the hosts in the secret.
//
// Matching is on registry host only. Docker config files in the wild write the host in several
// shapes — bare, with a scheme, with a trailing path — so each key is normalised rather than
// compared literally, which would silently fail to find a credential that is plainly there.
func (k *secretKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	want := res.RegistryStr()
	if a, ok := k.auths[want]; ok {
		return authn.FromConfig(a), nil
	}
	// Docker Hub is written half a dozen ways; accept the canonical index host too.
	if want == authn.DefaultAuthKey || want == "index.docker.io" || want == "docker.io" {
		for _, alias := range []string{authn.DefaultAuthKey, "index.docker.io", "docker.io"} {
			if a, ok := k.auths[alias]; ok {
				return authn.FromConfig(a), nil
			}
		}
	}
	// No credential for this host is not an error: an anonymous pull may still succeed, and
	// failing here would turn a public base image into a hard failure.
	return authn.Anonymous, nil
}

// keychainFromSecret builds a keychain from a kubernetes.io/dockerconfigjson Secret.
//
// Only that type is accepted. It is what `kubectl create secret docker-registry` produces and
// what an imagePullSecret already is, so users need not learn a bespoke format — and accepting
// loose username/password keys as well would mean two code paths for one job.
func keychainFromSecret(secret *corev1.Secret) (authn.Keychain, error) {
	raw, ok := secret.Data[corev1.DockerConfigJsonKey]
	if !ok {
		return nil, fmt.Errorf("missing key %q; create it with `kubectl create secret docker-registry`", corev1.DockerConfigJsonKey)
	}

	var cfg dockerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", corev1.DockerConfigJsonKey, err)
	}
	if len(cfg.Auths) == 0 {
		return nil, fmt.Errorf("%s contains no auths entries", corev1.DockerConfigJsonKey)
	}

	auths := make(map[string]authn.AuthConfig, len(cfg.Auths))
	for host, a := range cfg.Auths {
		user, pass := a.Username, a.Password
		if user == "" && a.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				return nil, fmt.Errorf("decoding auth for %q: %w", host, err)
			}
			user, pass, ok = strings.Cut(string(decoded), ":")
			if !ok {
				return nil, fmt.Errorf("auth for %q is not username:password", host)
			}
		}
		if user == "" && pass == "" {
			continue
		}
		auths[normaliseHost(host)] = authn.AuthConfig{Username: user, Password: pass}
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("%s contains no usable credentials", corev1.DockerConfigJsonKey)
	}
	return &secretKeychain{auths: auths}, nil
}

// normaliseHost strips scheme and path so "https://ghcr.io/v1/" and "ghcr.io" are the same key.
func normaliseHost(host string) string {
	h := host
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.Index(h, "/"); i > 0 {
		h = h[:i]
	}
	return h
}

// pullOptions builds remote options for pulling a base image.
//
// Separate from the push path deliberately: pulling a base image is a different credential with a
// different scope, and reusing spec.push.secretRef would silently send a push-scoped token to
// whatever registry the base happens to live in.
func (r *ImageCompositionReconciler) pullOptions(ctx context.Context, namespace string, ref *ociv1alpha1.LocalObjectReference) ([]remote.Option, error) {
	if ref == nil {
		return []remote.Option{remote.WithAuth(authn.Anonymous)}, nil
	}

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: namespace, Name: ref.Name}
	if err := r.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, terminal("pull secret %s not found", key)
		}
		return nil, fmt.Errorf("reading pull secret %s: %w", key, err)
	}

	kc, err := keychainFromSecret(&secret)
	if err != nil {
		return nil, terminal("pull secret %s: %v", key, err)
	}
	return []remote.Option{remote.WithAuthFromKeychain(kc)}, nil
}
