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
// minio-go is used rather than the AWS SDK because the target here is usually not AWS: Ceph RGW,
// MinIO, SeaweedFS. It defaults to path-style addressing against a custom endpoint, which is what
// those need, and it is a fraction of the dependency weight.
//
// This backend is optional. The disk backend remains the default and needs no configuration; S3
// is for keeping the input cache across restarts and across a controller being rescheduled onto
// a different node, which is where the cold-start re-fetch actually hurts.
type S3 struct {
	client *minio.Client
	bucket string
	prefix string
}

var (
	_ Store = (*S3)(nil)
)

// S3Config configures the S3 backend.
type S3Config struct {
	// Endpoint is the service URL, e.g. "https://s3.example.com". A scheme is required so that
	// TLS is an explicit choice rather than something inferred from a port number.
	Endpoint string

	// Bucket must already exist. The controller does not create it: bucket creation implies
	// policy decisions (versioning, lifecycle, encryption) that belong to whoever owns the
	// storage, not to a controller reconciling an unrelated object.
	Bucket string

	// Prefix scopes every key, so one bucket can be shared. Optional.
	Prefix string

	// Region. Many non-AWS implementations ignore it but require a value; Ceph RGW in
	// particular expects "default" rather than an AWS region name.
	Region string

	AccessKeyID     string
	SecretAccessKey string

	// PathStyle forces path-style addressing ("host/bucket/key") instead of virtual-host style
	// ("bucket.host/key"). Required by most self-hosted gateways, whose TLS certificate does not
	// cover per-bucket subdomains.
	PathStyle bool
}

// Validate reports configuration that cannot work, so a typo in chart values fails at startup
// rather than producing a controller that reports Ready and cannot store anything.
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
	// Credentials are checked as a pair: exactly one of them set is always a mistake, whereas
	// neither is legitimate when the gateway allows anonymous access or credentials come from
	// the environment.
	if (c.AccessKeyID == "") != (c.SecretAccessKey == "") {
		problems = append(problems, "access key ID and secret access key must be set together")
	}

	if len(problems) > 0 {
		return fmt.Errorf("s3 store: %s", strings.Join(problems, "; "))
	}
	return nil
}

// NewS3 creates an S3-backed store. It does not contact the endpoint; a bad endpoint surfaces on
// first use, and failing to start over a momentarily unreachable object store would be worse
// than starting and reporting unready.
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

// key maps a bucket object name back to a store key, so listings report the same keys callers
// passed in rather than prefixed ones.
func (s *S3) key(object string) string {
	if s.prefix == "" {
		return object
	}
	return strings.TrimPrefix(object, s.prefix+"/")
}

// notFound translates a missing-object error. minio-go reports it as a typed error response, and
// every caller treats a miss as ordinary control flow rather than a failure.
func notFound(err error) bool {
	return minio.ToErrorResponse(err).Code == "NoSuchKey" ||
		minio.ToErrorResponse(err).Code == "NoSuchBucket" ||
		minio.ToErrorResponse(err).StatusCode == 404
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
	// GetObject is lazy: it returns without contacting the server, so a missing object only
	// surfaces on the first read. Stat here so callers get ErrNotFound from Open, the way the
	// interface promises and the way the disk backend behaves.
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
	// Size -1 makes minio-go stream with a multipart upload rather than buffering the whole
	// object to learn its length. Layers run to hundreds of megabytes.
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

func (s *S3) List(ctx context.Context, prefix string) ([]Info, error) {
	var out []Info
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    s.object(prefix),
		Recursive: true,
	}) {
		if obj.Err != nil {
			// Return rather than continue. A partial listing is worse than no listing: garbage
			// collection would read it as "these objects do not exist" and delete live content.
			return nil, fmt.Errorf("listing %s: %w", prefix, obj.Err)
		}
		out = append(out, Info{
			Key:     s.key(obj.Key),
			Size:    obj.Size,
			ModTime: obj.LastModified,
		})
	}
	return out, nil
}
