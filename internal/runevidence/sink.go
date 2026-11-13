// SPDX-License-Identifier: Apache-2.0

package runevidence

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/stateracloud/koinon/internal/checkpoint"
)

// DirSink writes the objects as files under a directory (a prefix on a disk).
type DirSink string

// Put implements Sink.
func (d DirSink) Put(_ context.Context, name string, data []byte) error {
	if err := os.MkdirAll(string(d), 0o750); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(string(d), filepath.Base(name)), data, 0o600)
}

// S3Sink writes the objects under one prefix of an S3-compatible bucket, with the same bucket
// configuration the checkpoints use (checkpoint.S3).
type S3Sink struct {
	S3     checkpoint.S3
	Prefix string
}

// Put implements Sink.
func (s S3Sink) Put(ctx context.Context, name string, data []byte) error {
	host, plain := s.S3.Endpoint, false
	if strings.Contains(host, "://") {
		u, err := url.Parse(host)
		if err != nil || u.Host == "" {
			return fmt.Errorf("the S3 endpoint %q is not a host or URL", s.S3.Endpoint)
		}
		host, plain = u.Host, u.Scheme == "http"
	}
	c, err := minio.New(host, &minio.Options{
		Creds: credentials.NewStaticV4(s.S3.AccessKeyID, s.S3.SecretAccessKey, ""), Secure: !plain, Region: s.S3.Region,
	})
	if err != nil {
		return err
	}
	key := path.Join(strings.Trim(s.Prefix, "/"), path.Base(name))
	_, err = c.PutObject(ctx, s.S3.Bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: "application/x-ndjson"})
	return err
}

// ParsePrefix reads an --out value: a directory, or s3://bucket/prefix (the bucket must be the
// configured one; the endpoint and key come from s3).
func ParsePrefix(out string, s3 checkpoint.S3) (Sink, error) {
	if out == "" {
		return nil, errors.New("no output prefix")
	}
	if rest, ok := strings.CutPrefix(out, "s3://"); ok {
		bucket, prefix, _ := strings.Cut(rest, "/")
		if bucket == "" {
			return nil, fmt.Errorf("%q has no bucket", out)
		}
		s3.Bucket = bucket
		if err := s3.Validate(); err != nil {
			return nil, fmt.Errorf("the S3 settings for %q: %w", out, err)
		}
		return S3Sink{S3: s3, Prefix: prefix}, nil
	}
	return DirSink(out), nil
}
