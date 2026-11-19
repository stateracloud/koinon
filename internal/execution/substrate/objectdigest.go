// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/stateracloud/koinon/internal/execution"
)

// ObjectStore is the S3-compatible bucket that holds Substrate's snapshots (rustfs on the generic
// install, see docs/dev/substrate-install.md). The reader only reads: give it a read-only key (docs/dev/substrate-install.md).
// The credentials stay in Koinon's process and never reach a sandbox.
type ObjectStore struct {
	// Endpoint is a host:port or an http(s) URL; the scheme decides TLS (default https).
	Endpoint string
	Region   string
	// Bucket is the only bucket the reader opens. A snapshot URI that names another bucket is refused.
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
}

// ContentDigester returns the [Config.ContentDigest] function of the store.
//
// A snapshot URI addresses a prefix, not one object (ateapi ObjectSnapshot.snapshot_uri). The digest
// covers every object under it: the key, the size and the SHA-256 of the bytes, in key order. It
// reads all bytes on every call, so the cost grows with the snapshot, and a changed, added or
// removed object changes the digest. An ETag is not enough: a multipart upload has no content hash.
func (s ObjectStore) ContentDigester() (func(ctx context.Context, objectURI string) (string, error), error) {
	if s.Endpoint == "" || s.Bucket == "" {
		return nil, errors.New("substrate: the snapshot object store needs an endpoint and a bucket")
	}
	host, plain := s.Endpoint, false
	if strings.Contains(host, "://") {
		u, err := url.Parse(host)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("substrate: the snapshot object store endpoint %q is not a host or an http(s) URL", s.Endpoint)
		}
		host, plain = u.Host, u.Scheme == "http"
	}
	c, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(s.AccessKeyID, s.SecretAccessKey, ""),
		Secure: !plain,
		Region: s.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("substrate: the snapshot object store client: %w", err)
	}
	return func(ctx context.Context, objectURI string) (string, error) {
		return contentDigest(ctx, c, s.Bucket, objectURI)
	}, nil
}

// snapshotObjectPrefix splits a snapshot URI ("<scheme>://<bucket>/<prefix>") and checks the bucket.
func snapshotObjectPrefix(uri, bucket string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("substrate: the snapshot address %q is not <scheme>://<bucket>/<prefix>", uri)
	}
	if u.Host != bucket {
		return "", fmt.Errorf("substrate: the snapshot address %q names bucket %q, not the configured snapshot bucket", uri, u.Host)
	}
	p := strings.TrimPrefix(u.Path, "/")
	if p == "" {
		return "", fmt.Errorf("substrate: the snapshot address %q has no prefix: the digest never covers a whole bucket", uri)
	}
	if !strings.HasSuffix(p, "/") {
		p += "/" // a prefix match on "snap-1" must not read "snap-10"
	}
	return p, nil
}

// errNoObject marks a snapshot address that holds no object any more.
var errNoObject = errors.New("snapshot holds no object")

// storeError gives an object-store error its kind (execution.ErrNotFound, ErrPermissionDenied or
// ErrUnavailable), so the caller can tell a snapshot that is gone (start cold, ADR 0120 section 5.6)
// from a store outage. Errors of the driver's own check stay plain.
func storeError(op string, err error) error {
	var mr minio.ErrorResponse
	switch {
	case errors.Is(err, errNoObject), errors.As(err, &mr) && (mr.Code == "NoSuchKey" || mr.Code == "NoSuchBucket"):
		return fail(op, execution.ErrNotFound, "%v", err)
	case errors.As(err, &mr) && (mr.Code == "AccessDenied" || mr.Code == "InvalidAccessKeyId" || mr.Code == "SignatureDoesNotMatch"):
		return fail(op, execution.ErrPermissionDenied, "%v", err)
	case errors.As(err, &mr), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fail(op, execution.ErrUnavailable, "%v", err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func contentDigest(ctx context.Context, c *minio.Client, bucket, uri string) (string, error) {
	prefix, err := snapshotObjectPrefix(uri, bucket)
	if err != nil {
		return "", err
	}
	var keys []string
	for o := range c.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return "", fmt.Errorf("substrate: listing snapshot %s: %w", uri, o.Err)
		}
		keys = append(keys, o.Key)
	}
	if len(keys) == 0 {
		return "", fmt.Errorf("substrate: snapshot %s: %w", uri, errNoObject)
	}
	slices.Sort(keys)
	h := sha256.New()
	for _, k := range keys {
		sum, n, err := hashObject(ctx, c, bucket, k)
		if err != nil {
			return "", fmt.Errorf("substrate: reading snapshot object %s: %w", k, err)
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00%s\n", strings.TrimPrefix(k, prefix), n, sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashObject(ctx context.Context, c *minio.Client, bucket, key string) (string, int64, error) {
	o, err := c.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = o.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, o)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
