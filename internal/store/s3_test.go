package store

import (
	"strings"
	"testing"
)

// S3Config.Validate turns a typo in chart values into a startup failure.

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
		// Without a scheme, whether TLS was intended is ambiguous; guessing could ship
		// credentials in plaintext.
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

// TestS3ConfigAllowsAnonymous: neither credential set is legitimate.
func TestS3ConfigAllowsAnonymous(t *testing.T) {
	cfg := S3Config{Endpoint: "http://minio.test:9000", Bucket: "artifacts"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("anonymous config was rejected: %v", err)
	}
}

// TestS3PrefixRoundTrip: prefixing a key must not double a separator.
func TestS3PrefixRoundTrip(t *testing.T) {
	for _, prefix := range []string{"", "composer", "/composer/", "a/b"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			s := &S3{prefix: strings.Trim(prefix, "/")}
			key := MustKey(NamespaceInputs, "sha256:abcd")

			if !strings.HasSuffix(s.object(key), key) {
				t.Fatalf("object name %q does not end in the key %q", s.object(key), key)
			}
			if strings.Contains(s.object(key), "//") {
				t.Fatalf("object name has a doubled separator: %q", s.object(key))
			}
		})
	}
}

// TestNewS3RejectsBadConfig: construction fails rather than deferring to first use.
func TestNewS3RejectsBadConfig(t *testing.T) {
	if _, err := NewS3(S3Config{Endpoint: "not a url", Bucket: "b"}); err == nil {
		t.Fatal("NewS3 accepted an unparseable endpoint")
	}
	if _, err := NewS3(S3Config{Bucket: "b"}); err == nil {
		t.Fatal("NewS3 accepted a missing endpoint")
	}
}

// TestNewS3AcceptsCephRGWShape: path-style, https, and region "default".
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
