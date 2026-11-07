// SPDX-License-Identifier: Apache-2.0

// Command gatewayctl is throwaway spike code for step 4 of docs/spikes/openshell.md:
// it drives the OpenShell gateway through the Go SDK with the mTLS client bundle.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	"github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/NVIDIA/OpenShell/sdk/go/proto/sandboxv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	workspace = "spike-a"
	gitea     = "git.example.invalid"
	probe     = "registry.example.invalid/koinon/spike-probe:1"
	// orgLabel mirrors internal/project.Domain; this spike module cannot import the root module.
	orgLabel = "koinon.dev/org"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	mtls := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "openshell/gateways/k8s/mtls")
	c, err := v1.NewClient(v1.Config{
		Address: "127.0.0.1:18080",
		TLS: &v1.TLSConfig{
			CertFile: filepath.Join(mtls, "tls.crt"),
			KeyFile:  filepath.Join(mtls, "tls.key"),
			CAFile:   filepath.Join(mtls, "ca.crt"),
		},
	})
	must(err, "connect")
	defer c.Close()

	step := os.Args[1]
	switch step {
	case "lifecycle":
		lifecycle(ctx, c)
	case "provider":
		provider(ctx, c, mtls)
	case "workspace":
		workspaces(ctx, c)
	default:
		log.Fatalf("unknown step %q", step)
	}
}

// lifecycle covers 4.1, 4.2, 4.4 and 4.5.
func lifecycle(ctx context.Context, c *v1.Client) {
	const tmplName, name = "probe", "g1"

	_, _ = c.SandboxTemplates().Delete(ctx, workspace, tmplName)
	tmpl, err := c.SandboxTemplates().Create(ctx, workspace, &v1.SandboxWorkloadTemplate{
		Name: tmplName,
		Spec: v1.SandboxWorkloadTemplateSpec{Workload: &v1.SandboxWorkloadConfig{
			Image:     probe,
			Resources: &v1.SandboxResources{CPU: "500m", Memory: "512Mi"},
		}},
	})
	must(err, "4.2 create template")
	fmt.Printf("4.2 template %s (rv %d)\n", tmpl.Name, tmpl.ResourceVersion)

	t0 := time.Now()
	sb, err := c.CreateSandboxFromTemplate(ctx, workspace, name, tmplName, &v1.SandboxSpec{
		Providers:     []string{"gitea-bot"},
		Command:       []string{"sleep", "infinity"},
		Policy:        step3Policy(false),
		RestartPolicy: v1.SandboxRestartPolicy("never"),
	})
	must(err, "4.1 create from template")
	fmt.Printf("4.1 created %s id=%s from template=%v\n", sb.Name, sb.ID, sb.CreatedFromWorkloadTemplate)

	w, err := c.Sandboxes().Watch(ctx, workspace, name)
	must(err, "4.5 watch")
	events := make(chan string, 64)
	go func() {
		for ev := range w.ResultChan() {
			if ev.Err != nil {
				events <- fmt.Sprintf("%s err=%v", ev.Type, ev.Err)
				continue
			}
			events <- fmt.Sprintf("%s phase=%s policy=v%d", ev.Type, ev.Object.Status.Phase, ev.Object.Status.CurrentPolicyVersion)
		}
		close(events)
	}()

	_, err = c.Sandboxes().WaitReady(ctx, workspace, name)
	must(err, "4.1 wait ready")
	fmt.Printf("4.1 ready after %s\n", time.Since(t0).Round(time.Second))

	run(ctx, c, name, "4.4 v1 GET whoami (expect 000)", "curl -s -o /dev/null -w %{http_code} --max-time 5 https://whoami.example.invalid/")
	run(ctx, c, name, "4.4 v1 POST pulls (expect 403)", "curl -s -o /dev/null -w %{http_code} -X POST -H \"Authorization: Bearer $SPIKE_TOKEN\" https://"+gitea+"/api/v1/repos/koinon-spike/hello/pulls")

	res, err := c.Config().Update(ctx, workspace, &v1.ConfigUpdate{Name: name, Policy: step3Policy(true)})
	must(err, "4.4 update policy")
	fmt.Printf("4.4 policy update: %+v\n", *res)
	t1 := time.Now()
	for i := 0; i < 30; i++ {
		if out := run(ctx, c, name, "", "curl -s -o /dev/null -w %{http_code} --max-time 5 https://whoami.example.invalid/"); out == "200" {
			fmt.Printf("4.4 v2 GET whoami 200 after %s\n", time.Since(t1).Round(time.Second))
			break
		}
		time.Sleep(time.Second)
	}

	_, err = c.Sandboxes().Stop(ctx, workspace, name)
	must(err, "4.1 stop")
	_, err = c.Sandboxes().WaitStopped(ctx, workspace, name)
	must(err, "4.1 wait stopped")
	fmt.Println("4.1 stopped")
	_, err = c.Sandboxes().Start(ctx, workspace, name)
	must(err, "4.1 start")
	_, err = c.Sandboxes().WaitReady(ctx, workspace, name)
	must(err, "4.1 wait ready after start")
	fmt.Println("4.1 started")

	logs, err := c.Sandboxes().GetLogs(ctx, workspace, name, v1.WithLogLines(200))
	must(err, "4.5 logs")
	fmt.Printf("4.5 logs: %d lines (buffer %d)\n", len(logs.Lines), logs.BufferTotal)
	for _, l := range logs.Lines {
		if strings.Contains(l.Message, "whoami") || strings.Contains(l.Message, "DENIED") {
			fmt.Printf("    %s %s %.140s\n", l.Timestamp.Format(time.TimeOnly), l.Level, l.Message)
		}
	}

	_, err = c.Sandboxes().Delete(ctx, workspace, name)
	must(err, "4.1 delete")
	time.Sleep(5 * time.Second)
	w.Stop()
	fmt.Println("4.5 watch events:")
	for e := range events {
		fmt.Println("    " + e)
	}
}

// step3Policy is hack/spike/policies/step3.yaml as Go structs; withEcho adds whoami.
func step3Policy(withEcho bool) *v1.SandboxPolicy {
	bins := []v1.PolicyNetworkBinary{{Path: "/usr/bin/curl"}, {Path: "/usr/bin/git"}, {Path: "/usr/bin/python3.12"}}
	p := &v1.SandboxPolicy{
		Version: 1,
		Filesystem: &v1.FilesystemPolicy{
			IncludeWorkdir: true,
			ReadOnly:       []string{"/bin", "/usr", "/lib", "/proc", "/dev/urandom", "/etc", "/var/log"},
			ReadWrite:      []string{"/tmp", "/dev/null"},
		},
		Landlock: &v1.LandlockPolicy{Compatibility: "hard_requirement"},
		NetworkPolicies: map[string]v1.NetworkPolicyRule{
			"gitea_no_pr": {
				Name: "gitea_no_pr",
				Endpoints: []v1.PolicyNetworkEndpoint{{
					Host: gitea, Port: 443, Path: "/api/v1/**", Protocol: "rest",
					Enforcement: v1.NetworkEnforcementModeEnforce,
					Access:      v1.NetworkAccessPresetReadOnly,
					DenyRules:   []v1.L7DenyRule{{Method: "POST", Path: "/api/v1/repos/*/*/pulls"}},
				}},
				Binaries: bins,
			},
		},
	}
	if withEcho {
		p.NetworkPolicies["echo"] = v1.NetworkPolicyRule{
			Name: "echo",
			Endpoints: []v1.PolicyNetworkEndpoint{{
				Host: "whoami.example.invalid", Port: 443, Protocol: "rest",
				Enforcement: v1.NetworkEnforcementModeEnforce,
				Access:      v1.NetworkAccessPresetReadOnly,
			}},
			Binaries: bins[:1],
		}
	}
	return p
}

// provider covers 4.3: a custom profile for agentgateway, a provider with a
// credential, and a pushed update. The SDK's ProviderProfile endpoint type has
// only host, port and protocol, which the gateway rejects for L7 endpoints, so
// the profile goes through the generated gRPC client instead.
func provider(ctx context.Context, c *v1.Client, mtls string) {
	const profileID, name = "agentgateway-spike", "agentgateway-key"
	p := c.Providers()
	_, _ = p.Delete(ctx, workspace, name)
	_, _ = p.Profiles().Delete(ctx, workspace, profileID)

	raw := rawClient(mtls)
	resp, err := raw.ImportProviderProfiles(ctx, &openshellv1.ImportProviderProfilesRequest{
		WorkspaceScope: &datamodelv1.WorkspaceSelector{Selection: &datamodelv1.WorkspaceSelector_Workspace{Workspace: workspace}},
		Profiles: []*openshellv1.ProviderProfileImportItem{{Source: "gatewayctl", Profile: &openshellv1.ProviderProfile{
			Id:          profileID,
			DisplayName: "agentgateway (spike)",
			Description: "Per-agent key for the spike agentgateway",
			Category:    openshellv1.ProviderProfileCategory_PROVIDER_PROFILE_CATEGORY_INFERENCE,
			Credentials: []*openshellv1.ProviderProfileCredential{{
				Name: "api_key", EnvVars: []string{"OPENAI_API_KEY"}, Required: true,
				AuthStyle: "bearer", HeaderName: "authorization", StablePlaceholder: true,
			}},
			Endpoints: []*sandboxv1.NetworkEndpoint{{
				Host: "agentgateway.agentgateway-system.svc.cluster.local", Port: 8080, Protocol: "rest",
				Enforcement: sandboxv1.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_ENFORCE,
				Rules:       []*sandboxv1.L7Rule{{Allow: &sandboxv1.L7Allow{Method: "POST", Path: "/v1/chat/completions"}}},
			}},
			Binaries: []*sandboxv1.NetworkBinary{{Path: "/usr/bin/curl"}},
		}}},
	})
	must(err, "4.3 import profile")
	fmt.Printf("4.3 profile imported=%v diagnostics=%v\n", resp.Imported, resp.Diagnostics)

	created, err := p.Create(ctx, workspace, &v1.Provider{
		Name: name, Type: profileID,
		Spec: v1.ProviderSpec{ProfileWorkspace: workspace, Credentials: map[string]string{"OPENAI_API_KEY": "spike-key-v1"}},
	})
	must(err, "4.3 create provider")
	fmt.Printf("4.3 provider %s rv=%d credential keys=%v values returned=%v\n",
		created.Name, created.ResourceVersion, keys(created.Spec.Credentials), values(created.Spec.Credentials))

	created.Spec.Credentials = map[string]string{"OPENAI_API_KEY": "spike-key-v2"}
	updated, err := p.Update(ctx, workspace, created)
	must(err, "4.3 update provider")
	fmt.Printf("4.3 provider updated rv=%d\n", updated.ResourceVersion)

	got, err := p.Profiles().Get(ctx, workspace, profileID)
	must(err, "4.3 get profile")
	fmt.Printf("4.3 profile as the SDK reads it back: endpoints=%+v stable=%v\n", got.Endpoints, got.Credentials[0].StablePlaceholder)
}

func rawClient(mtls string) openshellv1.OpenShellClient {
	cert, err := tls.LoadX509KeyPair(filepath.Join(mtls, "tls.crt"), filepath.Join(mtls, "tls.key"))
	must(err, "load client cert")
	ca, err := os.ReadFile(filepath.Join(mtls, "ca.crt"))
	must(err, "read ca")
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	conn, err := grpc.NewClient("127.0.0.1:18080", grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS12,
	})))
	must(err, "raw grpc client")
	return openshellv1.NewOpenShellClient(conn)
}

func values(m map[string]string) bool {
	for _, v := range m {
		if v != "" {
			return true
		}
	}
	return false
}

// workspaces covers 4.6.
func workspaces(ctx context.Context, c *v1.Client) {
	ws := c.Workspaces()
	if _, err := ws.Get(ctx, "spike-b"); err != nil {
		_, err = ws.Create(ctx, "spike-b", map[string]string{orgLabel: "spike-b"})
		must(err, "4.6 create workspace")
	}
	m, err := ws.AddMember(ctx, "spike-b", "spike-b-admin", v1.WorkspaceRoleAdmin)
	must(err, "4.6 add member")
	fmt.Printf("4.6 member %s role=%s\n", m.Name, m.Role)
	all, err := ws.ListAllMembers(ctx, "spike-b")
	must(err, "4.6 list members")
	for _, m := range all {
		fmt.Printf("4.6   %s %s\n", m.Name, m.Role)
	}
}

func run(ctx context.Context, c *v1.Client, name, label, script string) string {
	res, err := c.Exec().Run(ctx, workspace, name, []string{"bash", "-c", script})
	out := ""
	if err == nil {
		out = strings.TrimSpace(string(res.Stdout))
	}
	if label != "" {
		fmt.Printf("%s: %s err=%v\n", label, out, err)
	}
	return out
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func must(err error, what string) {
	if err != nil {
		log.Fatalf("%s: %v", what, err)
	}
}
