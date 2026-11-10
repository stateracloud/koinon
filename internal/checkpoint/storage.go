// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	// Epochs lists, in one delimited listing of the agent's prefix (Prefix), the epochs after 0
	// whose folder (r<N>/) holds anything, in no particular order.
	Epochs(ctx context.Context, agentPrefix string) ([]int32, error)
}

// epochFolder matches a later epoch's folder under an agent's prefix, as a delimited listing names
// it (r<N>/) or as a directory entry names it (r<N>).
var epochFolder = regexp.MustCompile(`^r([1-9][0-9]{0,8})/?$`)

// epochOfFolder is the epoch whose folder name is, if it is one.
func epochOfFolder(name string) (int32, bool) {
	m := epochFolder.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 32)
	return int32(n), err == nil
}

// HeldEpochs lists the agent's repository epochs after 0 whose folder in the bucket holds anything,
// in ascending order: a repository, or what is left of one. It is one listing, so a repository
// started at r<N+3>/ is found even when r<N+2>/ is empty.
func (w *Writer) HeldEpochs(ctx context.Context, org, agent string) ([]int32, error) {
	epochs, err := w.Storage.Epochs(ctx, Prefix(org, agent))
	if err != nil {
		return nil, fmt.Errorf("checkpoint: listing the repository epochs of %s: %w", Prefix(org, agent), err)
	}
	slices.Sort(epochs)
	return slices.Compact(epochs), nil
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
// (the secret keys page of the docs site, project.SecretKeysDocURL, "Suspected compromise") the operator sees the restarted replicas use
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

// Epochs implements Storage. Without a delimiter kopia's S3 listing (minio-go's ListObjects, not
// recursive) names each folder under the prefix once, as r<N>/, and no blob of the agent's epoch 0
// repository starts with r (EpochPrefix).
func (s S3) Epochs(ctx context.Context, agentPrefix string) ([]int32, error) {
	st, err := s.Open(ctx, agentPrefix, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close(context.WithoutCancel(ctx)) }()
	var out []int32
	err = st.ListBlobs(ctx, "r", func(m blob.Metadata) error {
		if n, ok := epochOfFolder(string(m.BlobID)); ok {
			out = append(out, n)
		}
		return nil
	})
	return out, err
}

// Epochs implements Storage: the r<N> directories that hold a file.
func (d Dir) Epochs(_ context.Context, agentPrefix string) ([]int32, error) {
	root := filepath.Join(string(d), filepath.FromSlash(agentPrefix))
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []int32
	for _, e := range entries {
		n, ok := epochOfFolder(e.Name())
		if !ok || !e.IsDir() {
			continue
		}
		held := false
		err := filepath.WalkDir(filepath.Join(root, e.Name()), func(_ string, f fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if f.Type().IsRegular() {
				held = true
				return fs.SkipAll
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if held {
			out = append(out, n)
		}
	}
	return out, nil
}

// Dir keeps repositories in a directory, one folder per prefix. It is for tests and development.
type Dir string

// Open implements Storage.
func (d Dir) Open(ctx context.Context, prefix string, create bool) (blob.Storage, error) {
	return filesystem.New(ctx, &filesystem.Options{Path: filepath.Join(string(d), filepath.FromSlash(prefix))}, create)
}
