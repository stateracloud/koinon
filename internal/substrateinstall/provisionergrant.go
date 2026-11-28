// SPDX-License-Identifier: Apache-2.0

package substrateinstall

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
	"github.com/stateracloud/koinon/internal/substrateauthz"
)

const (
	// clientServiceAccount is Substrate's default bootstrap owner (docs/guides/authorization.md): ate-setup makes
	// it a bootstrap owner when the install names none.
	clientServiceAccount = "ate-client"
	// apiServerPort is the port of ate-api-server's gRPC listener (--grpc-listen-addr :443).
	apiServerPort = 443
	// apiTokenSeconds is the life of the token the installer mints: the grant takes seconds.
	apiTokenSeconds = 600
	// apiCASigner and apiCABundleLabel select the ClusterTrustBundle that holds the CA of ate-api-server's serving
	// certificate (Substrate manifests/ate-install/ate-api-server.yaml, volume servicedns).
	apiCASigner      = "servicedns.podcert.ate.dev/identity"
	apiCABundleLabel = "podcert.ate.dev/canarying=live"
)

// ControlDialer opens a connection to Substrate's control API as a principal that holds the global owner role.
// The returned func closes it. The installer's default is [Installer.openControl]; a test sets its own.
type ControlDialer func(ctx context.Context) (ateapiv1.ControlClient, func(), error)

// ProvisionerMember is the policy member of the provisioner's ServiceAccount (ADR 0128).
func (i *Installer) provisionerMember() (string, error) {
	if i.ProvisionerNamespace == "" || i.ProvisionerServiceAccount == "" {
		return "", fmt.Errorf("the installer needs the namespace and the ServiceAccount of the provisioner")
	}
	return substrateauthz.Member(i.ProvisionerNamespace, i.ProvisionerServiceAccount), nil
}

// EnsureProvisionerGrant makes the provisioner an owner in Substrate's global access policy (ADR 0128), as a
// revocable member and never as a bootstrap owner. It merges with the members that exist: a policy write replaces
// the whole policy. It writes nothing when the grant is there, so an upgrade runs it again, and an upgrade of an
// install from before the provisioner adds the grant. It reports whether it wrote.
func (i *Installer) EnsureProvisionerGrant(ctx context.Context) (bool, error) {
	member, err := i.provisionerMember()
	if err != nil {
		return false, err
	}
	dial := i.Control
	if dial == nil {
		dial = i.openControl
	}
	timeout := orDuration(i.WaitTimeout, 2*time.Minute)
	// One deadline bounds the whole grant: the dial, the policy calls and the retries between them.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		changed, err := i.grantOnce(ctx, dial, member)
		if err == nil {
			if changed {
				_, _ = fmt.Fprintf(i.Out, "Substrate global access policy: added %s as owner\n", member)
			}
			return changed, nil
		}
		// The api server restarts after the authentication ConfigMap changes: it is not reachable, or does not
		// know the issuer yet, for a short time.
		if c := status.Code(err); (c != codes.Unavailable && c != codes.Unauthenticated) || ctx.Err() != nil {
			return false, fmt.Errorf("grant the provisioner %s in Substrate's global access policy: %w", member, err)
		}
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("grant the provisioner %s in Substrate's global access policy: %w (last error: %w)", member, ctx.Err(), err)
		case <-time.After(orDuration(i.PollInterval, 2*time.Second)):
		}
	}
}

func (i *Installer) grantOnce(ctx context.Context, dial ControlDialer, member string) (bool, error) {
	ctl, closeFn, err := dial(ctx)
	if err != nil {
		return false, err
	}
	defer closeFn()
	changed, err := substrateauthz.EnsureGlobalMember(ctx, ctl, substrateauthz.RoleOwner, member)
	if err != nil {
		// EnsureGlobalMember wraps with %w, so the code survives for the retry above.
		return false, err
	}
	return changed, nil
}

// bearer sends a token as the bearer credential.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return true }

// openControl reaches ate-api-server through a port-forward and acts as ate-client, the default bootstrap owner
// of the install (or with the token in APITokenFile when the install names other bootstrap owners). The
// NetworkPolicy of ate-system admits only Substrate's own pods, and a port-forward is not ingress to the pod.
//
// The installer verifies the api server's serving certificate before it sends the token (apiServerTLS): the
// CA comes from the Kubernetes API, and the name checked is the Service DNS name that the certificate carries.
// It fails closed: no CA, no connection.
func (i *Installer) openControl(ctx context.Context) (ateapiv1.ControlClient, func(), error) {
	ns := i.Checker.Config.SubstrateOrDefault().Namespace
	var token string
	if i.APITokenFile != "" {
		b, err := os.ReadFile(i.APITokenFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read --api-token-file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	} else {
		exp := int64(apiTokenSeconds)
		tr, err := i.Kube.CoreV1().ServiceAccounts(ns).CreateToken(ctx, clientServiceAccount, &authnv1.TokenRequest{
			Spec: authnv1.TokenRequestSpec{Audiences: []string{apiAudience}, ExpirationSeconds: &exp},
		}, metav1.CreateOptions{})
		if err != nil {
			return nil, nil, fmt.Errorf("mint a token for ServiceAccount %s/%s: %w", ns, clientServiceAccount, err)
		}
		token = tr.Status.Token
	}
	pods, err := i.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + apiServerName})
	if err != nil {
		return nil, nil, fmt.Errorf("list the %s pods: %w", apiServerName, err)
	}
	var pod *corev1.Pod
	for k := range pods.Items {
		if p := &pods.Items[k]; p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil && podReady(p) {
			pod = p
			break
		}
	}
	if pod == nil {
		// Unavailable: the api server restarts after the authentication change, and the caller retries.
		return nil, nil, status.Errorf(codes.Unavailable, "no ready %s pod in namespace %s", apiServerName, ns)
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: i.Kubeconfig}, &clientcmd.ConfigOverrides{CurrentContext: i.Context}).ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("load the kubeconfig for the port-forward: %w", err)
	}
	tlsCfg, err := i.apiServerTLS(ctx, ns)
	if err != nil {
		return nil, nil, err
	}
	local, stop, err := forwardPod(ctx, cfg, i.Kube, ns, pod.Name, apiServerPort)
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "port-forward to %s: %v", apiServerName, err)
	}
	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", local),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithPerRPCCredentials(bearer(token)))
	if err != nil {
		stop()
		return nil, nil, err
	}
	return ateapiv1.NewControlClient(conn), func() { _ = conn.Close(); stop() }, nil
}

// apiServerTLS is the TLS configuration that checks ate-api-server's serving certificate. Substrate's
// servicedns signer issues that certificate with the DNS name of the Service (api.<namespace>.svc) and publishes
// its CA as a ClusterTrustBundle (signer servicedns.podcert.ate.dev/identity, label podcert.ate.dev/canarying=live):
// the bundle that the pods of Substrate mount to verify the same certificate. A port-forward ends at the pod,
// so the name to check is the Service name and not 127.0.0.1. APICAFile replaces the bundle. No CA is an error.
func (i *Installer) apiServerTLS(ctx context.Context, ns string) (*tls.Config, error) {
	pool := x509.NewCertPool()
	switch {
	case i.APICAFile != "":
		b, err := os.ReadFile(i.APICAFile)
		if err != nil {
			return nil, fmt.Errorf("read --api-ca-file: %w", err)
		}
		if !pool.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("--api-ca-file %s holds no PEM certificate", i.APICAFile)
		}
	default:
		list, err := i.Kube.CertificatesV1().ClusterTrustBundles().List(ctx, metav1.ListOptions{LabelSelector: apiCABundleLabel})
		if err != nil {
			return nil, fmt.Errorf("list the ClusterTrustBundles of signer %s: %w", apiCASigner, err)
		}
		found := false
		for k := range list.Items {
			if list.Items[k].Spec.SignerName == apiCASigner && pool.AppendCertsFromPEM([]byte(list.Items[k].Spec.TrustBundle)) {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("no ClusterTrustBundle of signer %s with label %s holds a CA: the installer will not send a token to a server whose certificate it cannot verify (pass the CA with --api-ca-file)", apiCASigner, apiCABundleLabel)
		}
	}
	return &tls.Config{RootCAs: pool, ServerName: fmt.Sprintf("api.%s.svc", ns), MinVersion: tls.VersionTLS12}, nil
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
