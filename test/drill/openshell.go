// SPDX-License-Identifier: Apache-2.0

package drill

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	datamodelv1 "github.com/stateracloud/koinon/internal/gen/openshell/datamodel/v1"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
)

// GatewayPort is the OpenShell gateway's gRPC port in its Service.
const GatewayPort = 8080

// Gateway is where an OpenShell gateway installed by the drill is reached.
type Gateway struct {
	Namespace string
	// Service is the gateway's Service; Address is "<service>.<namespace>.svc:8080" for Koinon's chart.
	Service string
	// ClientTLSSecret is the Secret with the client certificate, key and CA.
	ClientTLSSecret string
}

// Address is the gateway address for the chart's openshell.address value.
func (g Gateway) Address() string {
	return fmt.Sprintf("%s.%s.svc:%d", g.Service, g.Namespace, GatewayPort)
}

// DiscoverGateway finds the Service and the client TLS Secret the gateway chart made in ns, so the
// drill does not depend on how the chart names them for a release other than "openshell".
func DiscoverGateway(ctx context.Context, k *Kube, ns string) (Gateway, error) {
	g := Gateway{Namespace: ns}
	out, err := k.Kubectl(ctx, "-n", ns, "get", "svc", "-o", "json")
	if err != nil {
		return g, err
	}
	var svcs struct {
		Items []struct {
			Metadata struct{ Name string }
			Spec     struct {
				ClusterIP string
				Ports     []struct{ Port int }
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &svcs); err != nil {
		return g, err
	}
	for _, s := range svcs.Items {
		if s.Spec.ClusterIP == "None" {
			continue
		}
		for _, p := range s.Spec.Ports {
			if p.Port == GatewayPort {
				g.Service = s.Metadata.Name
			}
		}
	}
	out, err = k.Kubectl(ctx, "-n", ns, "get", "secret", "-o", "name")
	if err != nil {
		return g, err
	}
	for _, line := range strings.Fields(out) {
		if n := strings.TrimPrefix(line, "secret/"); strings.HasSuffix(n, "client-tls") {
			g.ClientTLSSecret = n
		}
	}
	if g.Service == "" || g.ClientTLSSecret == "" {
		return g, fmt.Errorf("gateway in %s: service %q and client TLS secret %q: both are needed", ns, g.Service, g.ClientTLSSecret)
	}
	return g, nil
}

// Object is one OpenShell object, as the isolation check compares it.
type Object struct {
	Kind            string `json:"kind"`
	ID              string `json:"id"`
	Name            string `json:"name"`
	ResourceVersion uint64 `json:"resourceVersion"`
}

// SnapshotGateway lists every sandbox and provider of the gateway, over mTLS through a
// port-forward. serverName is the name checked against the gateway's certificate.
func SnapshotGateway(ctx context.Context, k *Kube, g Gateway, serverName string) ([]Object, error) {
	if serverName == "" {
		serverName = g.Service + "." + g.Namespace + ".svc"
	}
	raw, err := k.Kubectl(ctx, "-n", g.Namespace, "get", "secret", g.ClientTLSSecret, "-o", "json")
	if err != nil {
		return nil, err
	}
	var sec struct{ Data map[string]string }
	if err := json.Unmarshal([]byte(raw), &sec); err != nil {
		return nil, err
	}
	dec := func(key string) ([]byte, error) {
		v, ok := sec.Data[key]
		if !ok {
			return nil, fmt.Errorf("secret %s/%s has no %s", g.Namespace, g.ClientTLSSecret, key)
		}
		return base64.StdEncoding.DecodeString(v)
	}
	crt, err := dec("tls.crt")
	if err != nil {
		return nil, err
	}
	key, err := dec("tls.key")
	if err != nil {
		return nil, err
	}
	ca, err := dec("ca.crt")
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(crt, key)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("secret %s/%s: ca.crt holds no certificate", g.Namespace, g.ClientTLSSecret)
	}
	addr, stop, err := k.PortForward(ctx, g.Namespace, "svc/"+g.Service, GatewayPort)
	if err != nil {
		return nil, err
	}
	defer stop()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{pair}, RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12,
	})))
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	c := openshellv1.NewOpenShellClient(conn)
	all := &datamodelv1.WorkspaceSelector{Selection: &datamodelv1.WorkspaceSelector_AllWorkspaces{AllWorkspaces: &datamodelv1.AllWorkspaces{}}}
	var out []Object
	token := ""
	for {
		res, err := c.ListSandboxes(ctx, &openshellv1.ListSandboxesRequest{WorkspaceScope: all, PageSize: 1000, PageToken: token})
		if err != nil {
			return nil, fmt.Errorf("list sandboxes: %w", err)
		}
		for _, s := range res.GetSandboxes() {
			m := s.GetMetadata()
			out = append(out, Object{"sandbox", m.GetId(), m.GetName(), m.GetResourceVersion()})
		}
		if token = res.GetNextPageToken(); token == "" {
			break
		}
	}
	for {
		res, err := c.ListProviders(ctx, &openshellv1.ListProvidersRequest{WorkspaceScope: all, PageSize: 1000, PageToken: token})
		if err != nil {
			return nil, fmt.Errorf("list providers: %w", err)
		}
		for _, p := range res.GetProviders() {
			m := p.GetMetadata()
			out = append(out, Object{"provider", m.GetId(), m.GetName(), m.GetResourceVersion()})
		}
		if token = res.GetNextPageToken(); token == "" {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind+out[i].ID < out[j].Kind+out[j].ID })
	return out, nil
}

// DiffObjects lists what changed between two snapshots of one gateway: an object that is gone, new,
// or has another resource version (a changed spec or status). Empty means unchanged.
func DiffObjects(before, after []Object) []string {
	idx := func(l []Object) map[string]Object {
		m := map[string]Object{}
		for _, o := range l {
			m[o.Kind+"/"+o.ID] = o
		}
		return m
	}
	b, a := idx(before), idx(after)
	var d []string
	for k, o := range b {
		n, ok := a[k]
		switch {
		case !ok:
			d = append(d, fmt.Sprintf("%s %q is gone", k, o.Name))
		case n.ResourceVersion != o.ResourceVersion:
			d = append(d, fmt.Sprintf("%s %q changed: resource version %d -> %d", k, o.Name, o.ResourceVersion, n.ResourceVersion))
		}
	}
	for k, o := range a {
		if _, ok := b[k]; !ok {
			d = append(d, fmt.Sprintf("%s %q is new", k, o.Name))
		}
	}
	slices.Sort(d)
	return d
}
