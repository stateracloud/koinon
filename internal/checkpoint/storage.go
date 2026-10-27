// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/blob/s3"
)

// Storage opens the blob storage of one repository, identified by its prefix
// (orgs/<org>/homes/<agent>/).
type Storage interface {
	Open(ctx context.Context, prefix string, create bool) (blob.Storage, error)
}

// Prefix is the storage prefix of an agent's repository.
func Prefix(org, agent string) string { return "orgs/" + org + "/homes/" + agent + "/" }

// S3 is an S3-compatible bucket: AWS S3, Garage, SeaweedFS, Ceph RGW and the like. The client is
// minio-go: it addresses a bucket path-style on every endpoint but AWS's and Google's, so there
// is no addressing switch to set (the chart's checkpoints.s3.pathStyle is checked at start-up).
// Its requests carry no checksum trailers, which is what stores that predate them need
// (AWS_REQUEST_CHECKSUM_CALCULATION=when_required in the AWS SDKs).
type S3 struct {
	// Endpoint is a host:port or an http(s) URL; the scheme decides TLS (default https).
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
}

// Validate reports a configuration that cannot work.
func (s S3) Validate() error {
	var errs []error
	if s.Endpoint == "" {
		errs = append(errs, errors.New("no endpoint"))
	}
	if s.Bucket == "" {
		errs = append(errs, errors.New("no bucket"))
	}
	if s.AccessKeyID == "" || s.SecretAccessKey == "" {
		errs = append(errs, errors.New("no access key"))
	}
	return errors.Join(errs...)
}

// Open implements Storage.
func (s S3) Open(ctx context.Context, prefix string, create bool) (blob.Storage, error) {
	host, plain := s.Endpoint, false
	if strings.Contains(host, "://") {
		u, err := url.Parse(host)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("the S3 endpoint %q is not a host or URL", s.Endpoint)
		}
		host, plain = u.Host, u.Scheme == "http"
	}
	return s3.New(ctx, &s3.Options{
		BucketName: s.Bucket, Prefix: prefix, Endpoint: host, DoNotUseTLS: plain,
		AccessKeyID: s.AccessKeyID, SecretAccessKey: s.SecretAccessKey, Region: s.Region,
	}, create)
}

// Dir keeps repositories in a directory, one folder per prefix. It is for tests and development.
type Dir string

// Open implements Storage.
func (d Dir) Open(ctx context.Context, prefix string, create bool) (blob.Storage, error) {
	return filesystem.New(ctx, &filesystem.Options{Path: filepath.Join(string(d), filepath.FromSlash(prefix))}, create)
}
