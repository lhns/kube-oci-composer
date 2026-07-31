package store

import (
	"strings"
	"testing"
)

// S3Config.Validate is what stands between a typo in chart values and a controller that starts,
// reports Ready, and silently cannot store anything. These tests pin the cases that must fail.

func TestS3ConfigRejectsBadInput(t *testing.T) {
	valid := S3Config{
		Endpoint:        "https://s3.example.com",
		Bucket:          "artifacts",
		Region:          "default",
		AccessKeyID:     "id",
		SecretAccessKey: "secret",
		PathStyle:       true,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid config was rejected: %v", err)
	}

	cases := map[string]struct {
		mutate func(*S3Config)
		want   string
	}{
		"no endpoint": {
			func(c *S3Config) { c.Endpoint = "" },
			"endpoint is required",
		},
		"no bucket": {
			func(c *S3Config) { c.Bucket = "" },
			"bucket is required",
		},
		// A bare host is the most likely typo, and it is genuinely ambiguous: without a scheme
		// there is no way to know whether TLS was intended. Guessing would mean silently
		// shipping credentials in plaintext.
		"endpoint without a scheme": {
			func(c *S3Config) { c.Endpoint = "s3.example.com" },
			"scheme",
		},
		"endpoint with the wrong scheme": {
			func(c *S3Config) { c.Endpoint = "s3://bucket" },
			"scheme",
		},
		"endpoint with no host": {
			func(c *S3Config) { c.Endpoint = "https://" },
			"no host",
		},
		"only an access key": {
			func(c *S3Config) { c.SecretAccessKey = "" },
			"must be set together",
		},
		"only a secret key": {
			func(c *S3Config) { c.AccessKeyID = "" },
			"must be set together",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			tc.mutate(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestS3ConfigAllowsAnonymous — a gateway may permit anonymous reads, and credentials may come
// from the environment. Neither key set is legitimate; exactly one is always a mistake.
func TestS3ConfigAllowsAnonymous(t *testing.T) {
	cfg := S3Config{Endpoint: "http://minio.test:9000", Bucket: "artifacts"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("anonymous config was rejected: %v", err)
	}
}

// TestS3PrefixRoundTrip — keys must survive prefixing and unprefixing unchanged, or a listing
// would report keys that no other method accepts and garbage collection would compare the live
// set against names that never match.
func TestS3PrefixRoundTrip(t *testing.T) {
	for _, prefix := range []string{"", "composer", "/composer/", "a/b"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			s := &S3{prefix: strings.Trim(prefix, "/")}
			key := MustKey(NamespaceBlobs, "sha256:abcd")

			if got := s.key(s.object(key)); got != key {
				t.Fatalf("key round-trip gave %q, want %q", got, key)
			}
			if strings.Contains(s.object(key), "//") {
				t.Fatalf("object name has a doubled separator: %q", s.object(key))
			}
		})
	}
}

// TestNewS3RejectsBadConfig — construction must fail rather than defer the problem to first use.
func TestNewS3RejectsBadConfig(t *testing.T) {
	if _, err := NewS3(S3Config{Endpoint: "not a url", Bucket: "b"}); err == nil {
		t.Fatal("NewS3 accepted an unparseable endpoint")
	}
	if _, err := NewS3(S3Config{Bucket: "b"}); err == nil {
		t.Fatal("NewS3 accepted a missing endpoint")
	}
}

// TestNewS3AcceptsCephRGWShape — the configuration this estate actually runs. Path-style, an
// https endpoint, and region "default" rather than an AWS region name.
func TestNewS3AcceptsCephRGWShape(t *testing.T) {
	s, err := NewS3(S3Config{
		Endpoint:        "https://s3.example.com",
		Bucket:          "artifacts",
		Prefix:          "kube-oci-composer",
		Region:          "default",
		AccessKeyID:     "id",
		SecretAccessKey: "secret",
		PathStyle:       true,
	})
	if err != nil {
		t.Fatalf("rejected a Ceph RGW style config: %v", err)
	}
	if s.bucket != "artifacts" {
		t.Fatalf("bucket is %q", s.bucket)
	}
	if s.prefix != "kube-oci-composer" {
		t.Fatalf("prefix is %q", s.prefix)
	}
}
