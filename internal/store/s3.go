package store

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 stores objects in an S3-compatible bucket.
//
// minio-go rather than the AWS SDK: the target is usually not AWS (Ceph RGW, MinIO, SeaweedFS),
// and it is far lighter. Optional; it keeps the input cache across restarts and reschedules.
type S3 struct {
	client *minio.Client
	bucket string
	prefix string
}

var _ Store = (*S3)(nil)

// S3Config configures the S3 backend.
type S3Config struct {
	// Endpoint is the service URL, e.g. "https://s3.example.com". A scheme is required so TLS is
	// an explicit choice.
	Endpoint string

	// Bucket must already exist: its policies (versioning, lifecycle, encryption) belong to
	// whoever owns the storage.
	Bucket string

	// Prefix scopes every key, so one bucket can be shared. Optional.
	Prefix string

	// Region. Many non-AWS implementations require but ignore it; Ceph RGW expects "default".
	Region string

	AccessKeyID     string
	SecretAccessKey string

	// PathStyle forces path-style addressing ("host/bucket/key") instead of virtual-host style
	// ("bucket.host/key"), as most self-hosted gateways need.
	PathStyle bool
}

// Validate reports configuration that cannot work, so a typo in chart values fails at startup.
func (c S3Config) Validate() error {
	var problems []string

	if c.Endpoint == "" {
		problems = append(problems, "endpoint is required")
	} else {
		u, err := url.Parse(c.Endpoint)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("endpoint %q is not a URL: %v", c.Endpoint, err))
		case u.Scheme != "http" && u.Scheme != "https":
			problems = append(problems, fmt.Sprintf(
				"endpoint %q needs an http:// or https:// scheme", c.Endpoint))
		case u.Host == "":
			problems = append(problems, fmt.Sprintf("endpoint %q has no host", c.Endpoint))
		}
	}
	if c.Bucket == "" {
		problems = append(problems, "bucket is required")
	}
	// Exactly one credential set is a mistake; neither is legitimate (anonymous access).
	if (c.AccessKeyID == "") != (c.SecretAccessKey == "") {
		problems = append(problems, "access key ID and secret access key must be set together")
	}

	if len(problems) > 0 {
		return fmt.Errorf("s3 store: %s", strings.Join(problems, "; "))
	}
	return nil
}

// NewS3 creates an S3-backed store. It does not contact the endpoint, so a momentarily unreachable
// store does not prevent startup.
func NewS3(cfg S3Config) (*S3, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("s3 store: parsing endpoint: %w", err)
	}

	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:       u.Scheme == "https",
		Region:       cfg.Region,
		BucketLookup: bucketLookup(cfg.PathStyle),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 store: %w", err)
	}

	return &S3{
		client: client,
		bucket: cfg.Bucket,
		prefix: strings.Trim(cfg.Prefix, "/"),
	}, nil
}

func bucketLookup(pathStyle bool) minio.BucketLookupType {
	if pathStyle {
		return minio.BucketLookupPath
	}
	return minio.BucketLookupAuto
}

// object maps a store key to a bucket object name.
func (s *S3) object(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

// notFound reports whether err is minio-go's missing-object (or bucket) response.
func notFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket" || resp.StatusCode == 404
}

func (s *S3) Stat(ctx context.Context, key string) (Info, error) {
	info, err := s.client.StatObject(ctx, s.bucket, s.object(key), minio.StatObjectOptions{})
	if err != nil {
		if notFound(err) {
			return Info{}, ErrNotFound
		}
		return Info{}, fmt.Errorf("stat %s: %w", key, err)
	}
	return Info{Key: key, Size: info.Size, ModTime: info.LastModified}, nil
}

func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.object(key), minio.GetObjectOptions{})
	if err != nil {
		if notFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open %s: %w", key, err)
	}
	// GetObject is lazy, so Stat now to return ErrNotFound from Open as the interface promises.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		if notFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open %s: %w", key, err)
	}
	return obj, nil
}

func (s *S3) Write(ctx context.Context, key string, r io.Reader) error {
	// Size -1 streams a multipart upload instead of buffering the whole (possibly huge) layer.
	_, err := s.client.PutObject(ctx, s.bucket, s.object(key), r, -1, minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("write %s: %w", key, err)
	}
	return nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	err := s.client.RemoveObject(ctx, s.bucket, s.object(key), minio.RemoveObjectOptions{})
	if err != nil && !notFound(err) {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}
