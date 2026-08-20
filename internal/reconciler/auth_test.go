package reconciler

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func dockerSecret(t *testing.T, body string) *corev1.Secret {
	t.Helper()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(body)},
	}
}

func resolve(t *testing.T, kc authn.Keychain, ref string) authn.AuthConfig {
	t.Helper()
	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatalf("parsing %s: %v", ref, err)
	}
	a, err := kc.Resolve(parsed.Context())
	if err != nil {
		t.Fatalf("resolving %s: %v", ref, err)
	}
	cfg, err := a.Authorization()
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	return *cfg
}

// TestKeychainReadsUsernamePassword — the plain shape.
func TestKeychainReadsUsernamePassword(t *testing.T) {
	kc, err := KeychainFromSecret(dockerSecret(t,
		`{"auths":{"ghcr.io":{"username":"u","password":"p"}}}`))
	if err != nil {
		t.Fatalf("building keychain: %v", err)
	}
	got := resolve(t, kc, "ghcr.io/example/artifact:tag")
	if got.Username != "u" || got.Password != "p" {
		t.Fatalf("got %q/%q, want u/p", got.Username, got.Password)
	}
}

// TestKeychainDecodesAuthField — `kubectl create secret docker-registry` writes the base64 auth
// field and often no username at all, so ignoring it would break the most common way of making
// the secret.
func TestKeychainDecodesAuthField(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("u:p"))
	kc, err := KeychainFromSecret(dockerSecret(t,
		fmt.Sprintf(`{"auths":{"ghcr.io":{"auth":%q}}}`, enc)))
	if err != nil {
		t.Fatalf("building keychain: %v", err)
	}
	got := resolve(t, kc, "ghcr.io/example/artifact:tag")
	if got.Username != "u" || got.Password != "p" {
		t.Fatalf("got %q/%q, want u/p", got.Username, got.Password)
	}
}

// TestKeychainNormalisesHosts — docker config files write hosts with schemes and paths. Literal
// comparison would silently fail to find a credential that is plainly there, and the resulting
// 401 would look like a wrong password rather than a lookup miss.
func TestKeychainNormalisesHosts(t *testing.T) {
	kc, err := KeychainFromSecret(dockerSecret(t,
		`{"auths":{"https://registry.example.com/v1/":{"username":"u","password":"p"}}}`))
	if err != nil {
		t.Fatalf("building keychain: %v", err)
	}
	got := resolve(t, kc, "registry.example.com/example/artifact:tag")
	if got.Username != "u" {
		t.Fatalf("host with scheme and path did not match; got %q", got.Username)
	}
}

// TestKeychainFallsBackToAnonymous — an unrelated host must not be a hard failure, or a public
// base image would stop working the moment a push secret is attached.
func TestKeychainFallsBackToAnonymous(t *testing.T) {
	kc, err := KeychainFromSecret(dockerSecret(t,
		`{"auths":{"ghcr.io":{"username":"u","password":"p"}}}`))
	if err != nil {
		t.Fatalf("building keychain: %v", err)
	}
	got := resolve(t, kc, "quay.io/example/artifact:tag")
	if got.Username != "" || got.Password != "" {
		t.Fatalf("expected anonymous for an unlisted host, got %q/%q", got.Username, got.Password)
	}
}

// TestKeychainRejectsBadSecrets — misconfiguration should be reported, not silently treated as
// anonymous, because an anonymous push fails much later and much less clearly.
func TestKeychainRejectsBadSecrets(t *testing.T) {
	cases := map[string]*corev1.Secret{
		"missing key": {
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
			Data:       map[string][]byte{"username": []byte("u")},
		},
		"not json":     dockerSecret(t, `not json`),
		"no auths":     dockerSecret(t, `{"auths":{}}`),
		"empty creds":  dockerSecret(t, `{"auths":{"ghcr.io":{}}}`),
		"bad base64":   dockerSecret(t, `{"auths":{"ghcr.io":{"auth":"!!!"}}}`),
		"no separator": dockerSecret(t, fmt.Sprintf(`{"auths":{"ghcr.io":{"auth":%q}}}`, base64.StdEncoding.EncodeToString([]byte("nocolon")))),
	}
	for name, secret := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := KeychainFromSecret(secret); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
