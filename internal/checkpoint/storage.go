// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/blob/s3"
)

// Storage opens the blob storage of one repository, identified by its prefix
// (orgs/<org>/homes/<agent>/, or orgs/<org>/homes/<agent>/r<N>/ for a later repository epoch).
type Storage interface {
	Open(ctx context.Context, prefix string, create bool) (blob.Storage, error)
}

// Prefix is the storage prefix of an agent's first repository (epoch 0).
func Prefix(org, agent string) string { return "orgs/" + org + "/homes/" + agent + "/" }

// EpochPrefix is the storage prefix of an agent's repository of the given epoch: Prefix for epoch
// 0, and r<N>/ under it for each later one. kopia names its own blobs with fixed prefixes (p, q, x,
// n, kopia.*; repo.GetLockingStoragePrefixes) and its maintenance lists only those, none of them r,
// so the repository at Prefix never keeps or deletes a later epoch's blobs. Only a listing of every
// blob (kopia blob list) sees them.
func EpochPrefix(org, agent string, epoch int32) string {
	if epoch == 0 {
		return Prefix(org, agent)
	}
	return Prefix(org, agent) + "r" + strconv.FormatInt(int64(epoch), 10) + "/"
}

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

// AccessKeyFingerprint names the access key without showing it: the first 16 hex digits of the
// SHA-256 of its ID. koinon-agentd logs it at start, so that after the bucket's key is rotated
// (docs/ops/secret-keys.md, "Suspected compromise") the operator sees the restarted replicas use
// the new one. Empty without a key ID.
func (s S3) AccessKeyFingerprint() string {
	if s.AccessKeyID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s.AccessKeyID))
	return "sha256:" + hex.EncodeToString(sum[:8])
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
