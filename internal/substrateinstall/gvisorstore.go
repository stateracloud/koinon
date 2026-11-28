// SPDX-License-Identifier: Apache-2.0

package substrateinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// rustFSRegion is the region rustfs reports; its bucket-init Job uses the same value.
const rustFSRegion = "us-east-1"

// ErrNoBucket is returned by the store of the cluster when its bucket does not exist (yet). On kind the
// bucket-init Job of the object store makes it a few seconds after rustfs starts, so the installer retries
// on this error before it gives up.
var ErrNoBucket = errors.New("the bucket does not exist in the cluster object store")

// minioStore is an AssetStore over an S3 endpoint.
type minioStore struct {
	c      *minio.Client
	bucket string
}

// NewS3AssetStore returns an AssetStore for one bucket of an S3-compatible endpoint (a host:port or an
// http(s) URL; no scheme means https). The keys stay in the process of the installer.
func NewS3AssetStore(endpoint, bucket, accessKey, secretKey string) (AssetStore, error) {
	host, plain := endpoint, false
	if strings.Contains(host, "://") {
		u, err := url.Parse(host)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("the object store endpoint %q is not a host or an http(s) URL", endpoint)
		}
		host, plain = u.Host, u.Scheme == "http"
	}
	c, err := minio.New(host, &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, ""), Secure: !plain, Region: rustFSRegion})
	if err != nil {
		return nil, fmt.Errorf("the object store client: %w", err)
	}
	return minioStore{c: c, bucket: bucket}, nil
}

func (s minioStore) Digest(ctx context.Context, key string) (string, bool, error) {
	obj, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return "", false, err
	}
	defer func() { _ = obj.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, obj); err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return "", false, nil
		}
		if minio.ToErrorResponse(err).Code == "NoSuchBucket" {
			return "", false, fmt.Errorf("the bucket %q does not exist in the cluster object store (the platform creates only its own snapshot bucket): %w: %w", s.bucket, ErrNoBucket, err)
		}
		return "", false, err
	}
	return hex.EncodeToString(h.Sum(nil)), true, nil
}

func (s minioStore) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	_, err := s.c.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{ContentType: "application/zstd"})
	return err
}

// httpFetcher opens a tarball with a plain GET. The pinned digest protects the content, so http is allowed
// for an operator mirror inside the network.
type httpFetcher struct{ client *http.Client }

// NewHTTPFetcher returns the default AssetFetcher.
func NewHTTPFetcher() AssetFetcher {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = time.Minute
	return httpFetcher{client: &http.Client{Transport: t}}
}

func (f httpFetcher) Open(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%q is not an http(s) URL", redactURL(rawURL))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return resp.Body, nil
}

// rustFSCredentials reads the keys the rustfs Deployment runs with: a literal value (the kind overlay) or a
// Secret reference (the generic platform, EnsureRustFSCredentials). The installer reads the cluster; it
// never writes the keys anywhere.
func (i *Installer) rustFSCredentials(ctx context.Context, ns string) (accessKey, secretKey string, err error) {
	dep, err := i.Kube.AppsV1().Deployments(ns).Get(ctx, "rustfs", metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("read Deployment %s/rustfs: %w", ns, err)
	}
	var env []corev1.EnvVar
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == "rustfs" {
			env = c.Env
		}
	}
	resolve := func(name string) (string, error) {
		for _, e := range env {
			if e.Name != name {
				continue
			}
			if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
				return e.Value, nil
			}
			ref := e.ValueFrom.SecretKeyRef
			s, err := i.Kube.CoreV1().Secrets(ns).Get(ctx, ref.Name, metav1.GetOptions{})
			if err != nil {
				return "", fmt.Errorf("read Secret %s/%s: %w", ns, ref.Name, err)
			}
			return string(s.Data[ref.Key]), nil
		}
		return "", fmt.Errorf("the rustfs Deployment has no %s", name)
	}
	if accessKey, err = resolve("RUSTFS_ACCESS_KEY"); err != nil {
		return "", "", err
	}
	if secretKey, err = resolve("RUSTFS_SECRET_KEY"); err != nil {
		return "", "", err
	}
	if accessKey == "" || secretKey == "" {
		return "", "", errors.New("the rustfs Deployment has an empty access key or secret key")
	}
	return accessKey, secretKey, nil
}

// openClusterStore opens the object store of the cluster through a port-forward to its rustfs pod: the
// NetworkPolicy of ate-system lets only Substrate's own pods reach the S3 API, and a port-forward is not
// ingress to the pod. The returned func closes the forward.
func (i *Installer) openClusterStore(ctx context.Context) (AssetStore, func(), error) {
	ns := i.Checker.Config.SubstrateOrDefault().Namespace
	access, secret, err := i.rustFSCredentials(ctx, ns)
	if err != nil {
		return nil, nil, err
	}
	pods, err := i.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=rustfs"})
	if err != nil {
		return nil, nil, fmt.Errorf("list the rustfs pods: %w", err)
	}
	var pod *corev1.Pod
	for k := range pods.Items {
		p := &pods.Items[k]
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			pod = p
			break
		}
	}
	if pod == nil {
		return nil, nil, fmt.Errorf("no running rustfs pod in namespace %s: the cluster object store is not up", ns)
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: i.Kubeconfig}, &clientcmd.ConfigOverrides{CurrentContext: i.Context}).ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("load the kubeconfig for the port-forward: %w", err)
	}
	local, stop, err := forwardPod(ctx, cfg, i.Kube, ns, pod.Name, 9000)
	if err != nil {
		return nil, nil, fmt.Errorf("port-forward to the cluster object store: %w", err)
	}
	store, err := NewS3AssetStore(fmt.Sprintf("http://127.0.0.1:%d", local), i.Profile.StorageBucket, access, secret)
	if err != nil {
		stop()
		return nil, nil, err
	}
	return store, stop, nil
}

// forwardPod forwards a free local port to a pod port and returns it once the tunnel is ready.
func forwardPod(ctx context.Context, cfg *rest.Config, kube kubernetes.Interface, ns, pod string, remote int) (int, func(), error) {
	rt, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return 0, nil, err
	}
	req := kube.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("portforward")
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: rt}, http.MethodPost, req.URL())
	stopCh, readyCh := make(chan struct{}), make(chan struct{})
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", remote)}, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return 0, nil, err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- fw.ForwardPorts() }()
	stop := func() { close(stopCh) }
	select {
	case <-readyCh:
	case err := <-errCh:
		return 0, nil, err
	case <-ctx.Done():
		stop()
		return 0, nil, ctx.Err()
	case <-time.After(30 * time.Second):
		stop()
		return 0, nil, errors.New("the port-forward was not ready in 30 seconds")
	}
	ports, err := fw.GetPorts()
	if err == nil && len(ports) != 1 {
		err = errors.New("the port-forward did not report one port")
	}
	if err != nil {
		stop()
		return 0, nil, fmt.Errorf("the port-forward reports no port: %w", err)
	}
	return int(ports[0].Local), stop, nil
}
