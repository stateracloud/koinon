// SPDX-License-Identifier: Apache-2.0

// Package s3test starts a real S3 server for tests: the SeaweedFS of the spike (ADR 0032), plain
// HTTP and path-style, with the bucket the checkpoint writer uses. The image is pulled through the
// Docker Hub prefix when TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX is set.
package s3test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/testing/containerhost"
)

// Image is the S3 server: plain HTTP, path-style, no checksum trailers.
const Image = "chrislusf/seaweedfs:4.50"

const identities = `{"identities":[{"name":"test","credentials":[{"accessKey":"koinon_test_not-a-real-key","secretKey":"koinon_test_not-a-real-secret"}],"actions":["Admin","Read","Write","List","Tagging"]}]}`

// Start runs the server and returns the checkpoint.S3 that reaches it once it serves the calls the
// writer makes. The container ends with the calling spec's cleanup (cleanup).
func Start(ctx context.Context, cleanup func(func(context.Context) error)) (checkpoint.S3, error) {
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: Image,
			// -volume.minFreeSpace: the default (1% of the disk) makes every volume read-only, and every
			// write fail, on a nearly full CI or dev disk.
			Cmd:          []string{"server", "-s3", "-s3.config=/etc/seaweedfs/s3.json", "-dir=/data", "-volume.max=8", "-master.volumeSizeLimitMB=64", "-volume.minFreeSpace=100MiB"},
			ExposedPorts: []string{"8333/tcp"},
			Files: []testcontainers.ContainerFile{{
				Reader: strings.NewReader(identities), ContainerFilePath: "/etc/seaweedfs/s3.json", FileMode: 0o644,
			}},
			WaitingFor: wait.ForListeningPort("8333/tcp").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return checkpoint.S3{}, err
	}
	cleanup(func(c context.Context) error { return ctr.Terminate(c) })
	// SeaweedFS listens on IPv4 only (old issue 290): dial the IPv4 loopback.
	host, err := containerhost.Host(ctx, ctr)
	if err != nil {
		return checkpoint.S3{}, err
	}
	port, err := ctr.MappedPort(ctx, "8333/tcp")
	if err != nil {
		return checkpoint.S3{}, err
	}
	s3 := checkpoint.S3{
		Endpoint: fmt.Sprintf("http://%s:%s", host, port.Port()), Region: "us-east-1", Bucket: "koinon-homes",
		AccessKeyID: "koinon_test_not-a-real-key", SecretAccessKey: "koinon_test_not-a-real-secret",
	}
	if err := s3.Validate(); err != nil {
		return s3, err
	}
	mc, err := minio.New(fmt.Sprintf("%s:%s", host, port.Port()), &minio.Options{Creds: credentials.NewStaticV4(s3.AccessKeyID, s3.SecretAccessKey, "")})
	if err != nil {
		return s3, err
	}
	// The port answers before the S3 gateway, the filer and the volume servers are ready (#37):
	// wait on real S3 calls succeeding, with a deadline per attempt so a hung call (a half-open
	// connection after a reset) cannot stall the wait.
	var last error
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		actx, cancel := context.WithTimeout(ctx, 10*time.Second)
		last = ready(actx, mc, s3.Bucket, s3.Region)
		cancel()
		if last == nil {
			return s3, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return s3, fmt.Errorf("the S3 server did not become ready: %w", last)
}

// ready reports nil once the server serves the calls the writer makes: it creates the bucket
// when missing, then writes, reads and deletes a probe object.
func ready(ctx context.Context, mc *minio.Client, bucket, region string) error {
	if err := mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: region}); err != nil {
		if resp := minio.ToErrorResponse(err); resp.Code != "BucketAlreadyOwnedByYou" && resp.Code != "BucketAlreadyExists" {
			return err
		}
	}
	const probe = ".readiness-probe"
	if _, err := mc.PutObject(ctx, bucket, probe, strings.NewReader("ok"), 2, minio.PutObjectOptions{}); err != nil {
		return err
	}
	obj, err := mc.GetObject(ctx, bucket, probe, minio.GetObjectOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = obj.Close() }()
	if _, err := io.Copy(io.Discard, obj); err != nil {
		return err
	}
	return mc.RemoveObject(ctx, bucket, probe, minio.RemoveObjectOptions{})
}
