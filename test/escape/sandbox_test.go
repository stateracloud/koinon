// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"context"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/templates"
)

//go:embed testdata/probe.py
var probeScript string

// check is one probe check (testdata/probe.py).
type check map[string]any

// target is a host:port the probe connects to over TCP.
type target struct {
	host string
	port int
}

func (t target) String() string { return net.JoinHostPort(t.host, strconv.Itoa(t.port)) }

// targets parses a comma-separated host:port list; IPv6 hosts go in brackets.
func targets(list string) []target {
	GinkgoHelper()
	var out []target
	for _, s := range strings.Split(list, ",") {
		host, port, err := net.SplitHostPort(strings.TrimSpace(s))
		Expect(err).NotTo(HaveOccurred(), s)
		p, err := strconv.Atoi(port)
		Expect(err).NotTo(HaveOccurred(), s)
		out = append(out, target{host, p})
	}
	return out
}

// The synthetic range OpenShell's policy DNS answers with for a name no policy entry matches,
// without asking an upstream resolver (spike 8.3).
var synthetic = netip.MustParsePrefix("198.18.0.0/15")

// scanRoots are the directories the secret scan walks: everything the sandbox policy lets the
// agent read, the home volume, and the usual places a credential could be left.
func scanRoots() []string {
	fs := reconciler.SandboxPolicy(nil).Filesystem
	roots := append(slices.Clone(fs.ReadOnly), fs.ReadWrite...)
	return append(roots, "/sandbox", "/run", "/var", "/home", "/root", "/srv", "/mnt", "/.openshell")
}

// credentialName is an environment variable name that suggests a credential.
var credentialName = regexp.MustCompile(`(?i)token|secret|passw|credential|auth|api_?key|private`)

// An agent sandbox as the reconciler renders it, with the agentgateway key and a GitHub read
// grant attached, against a real gateway: ADR 0025's cases (no direct egress, no cloud metadata,
// no service-account token, no secrets on disk or in the environment, placeholders only) and the
// spike 8 checks behind them. Every denial must come from the sandbox (EACCES at connect), not
// from a network that happens to be unreachable.
var _ = Describe("Agent sandbox", Label("escape", "openshell"), Ordered, ContinueOnFailure, func() {
	var (
		g          *gateway
		n          = suffix()
		sandbox    = fmt.Sprintf("kesa-%d", n)
		keyRecord  = fmt.Sprintf("kesak-%d", n)
		ghRecord   = fmt.Sprintf("kesag-%d", n)
		repo       = envOr("KOINON_ESCAPE_REPO", "octocat/Hello-World")
		gatewayKey = fmt.Sprintf("koinon-escape-key-%d-%x", n, time.Now().UnixNano())
		githubKey  = fmt.Sprintf("koinon-escape-gh-%d-%x", n, time.Now().UnixNano())
		clusterDNS = envOr("KOINON_ESCAPE_CLUSTER_DNS", "10.96.0.10")
	)

	// probe runs checks in the sandbox and returns each check's result by id.
	probe := func(ctx context.Context, checks ...check) map[string]any {
		GinkgoHelper()
		cfg, err := json.Marshal(checks)
		Expect(err).NotTo(HaveOccurred())
		script := fmt.Sprintf("import json\nCHECKS = json.loads(bytes.fromhex(%q))\n%s", hex.EncodeToString(cfg), probeScript)
		out, stderr, code := g.exec(ctx, sandbox, script, "python3", "-")
		Expect(code).To(Equal(0), stderr)
		var res map[string]any
		Expect(json.Unmarshal([]byte(out), &res)).To(Succeed(), out)
		for _, c := range checks {
			if c["op"] != "scan" && c["op"] != "env" {
				GinkgoWriter.Printf("%s -> %v\n", c["id"], res[c["id"].(string)])
			}
		}
		return res
	}
	tcp := func(t target) check {
		return check{"id": "tcp " + t.String(), "op": "tcp", "host": t.host, "port": t.port}
	}
	// deniedAtConnect asserts that every TCP check was refused by the sandbox.
	deniedAtConnect := func(ctx context.Context, ts []target) {
		GinkgoHelper()
		checks := make([]check, 0, len(ts))
		for _, t := range ts {
			checks = append(checks, tcp(t))
		}
		res := probe(ctx, checks...)
		for _, c := range checks {
			Expect(res[c["id"].(string)]).To(Equal("EACCES"), c["id"])
		}
	}

	BeforeAll(func(ctx SpecContext) {
		g = dialGateway(ctx)

		key := reconciler.GatewayProfile(store.AgentFlavorOpencode,
			reconciler.Flavor{Binaries: reconciler.DefaultBinaries[store.AgentFlavorOpencode]},
			reconciler.Endpoint{Host: "agentgateway.koinon-escape.invalid", Port: 4000}, true)
		key.ID = fmt.Sprintf("koinon-escape-agentgateway-%d", n)
		g.provider(ctx, execution.Provider{
			Name: keyRecord, Profile: key, Credentials: map[string]string{reconciler.GatewayKeyEnvVar: gatewayKey},
		})

		read, ok, err := reconciler.GitHubReadAccess(agentconfig.Config{
			Grants: []templates.Grant{{Connector: "github", Scopes: []string{"repo.read"}, Preset: templates.PresetStandard}},
			Bindings: []agentconfig.Binding{{
				Connector: "github", ConnectionID: "00000000-0000-0000-0000-000000000000", Targets: []string{repo},
			}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		binaries := append(slices.Clone(reconciler.DefaultGitBinaries), "/usr/bin/curl")
		creds := map[string]string{}
		for _, env := range reconciler.GitHubTokenEnvVars {
			creds[env] = githubKey
		}
		g.provider(ctx, execution.Provider{
			Name: ghRecord, Profile: reconciler.GitHubProfile(fmt.Sprintf("koinon-escape-gh-%d", n), read, binaries), Credentials: creds,
		})

		g.sandbox(ctx, execution.SandboxSpec{
			Name: sandbox, Providers: []string{keyRecord, ghRecord},
			Policy: reconciler.SandboxPolicy(map[string]execution.NetworkRule{"github_read": reconciler.GitHubRule(read, binaries)}),
		})
	}, NodeTimeout(6*time.Minute))

	It("reaches the granted endpoint (control for the denials below)", func(ctx SpecContext) {
		code, body, _ := g.curl(ctx, sandbox, "GET", "https://api.github.com/repos/"+repo)
		// GitHub itself answers: 401 for the dummy token, 200 if it ignores it.
		Expect(code).To(BeElementOf("200", "401"), body)
	}, NodeTimeout(2*time.Minute))

	It("denies direct egress to hosts and IPs outside the policy", func(ctx SpecContext) {
		ts := targets(envOr("KOINON_ESCAPE_EGRESS",
			"1.1.1.1:443,8.8.8.8:443,example.com:443,example.com:80,[2606:4700:4700::1111]:443,github.com:22"))
		deniedAtConnect(ctx, ts)

		By("an allowed binary (curl) gets no response either")
		for _, url := range []string{"https://example.com/", "https://1.1.1.1/"} {
			code, _, exit := g.curl(ctx, sandbox, "GET", url)
			Expect(code).To(Equal("000"), url)
			Expect(exit).NotTo(Equal(0), url)
		}
	}, NodeTimeout(3*time.Minute))

	It("denies DNS to any resolver but the sandbox's own, which answers only synthetic addresses", func(ctx SpecContext) {
		resolvers := []string{"8.8.8.8", "1.1.1.1", clusterDNS}
		var checks []check
		for _, h := range resolvers {
			checks = append(checks,
				check{"id": "udp " + h, "op": "udp_dns", "host": h},
				tcp(target{h, 53}))
		}
		// Granted names too: OpenShell's policy DNS answers every name from the synthetic range
		// and maps the address back to the name at the proxy, so a lookup tells the agent
		// nothing about the network (spike 8.3).
		names := []string{"example.com", fmt.Sprintf("koinon-escape-%d.example.com", n), "github.com"}
		for _, name := range names {
			checks = append(checks, check{"id": "resolve " + name, "op": "resolve", "name": name})
		}
		res := probe(ctx, checks...)
		for _, h := range resolvers {
			// The kernel refuses the datagram (EDESTADDRREQ from sendto, EACCES from connect).
			Expect(res["udp "+h]).To(BeElementOf("EDESTADDRREQ", "EACCES"), "udp "+h)
			Expect(res["tcp "+target{h, 53}.String()]).To(Equal("EACCES"), "tcp "+h)
		}
		for _, name := range names {
			addrs, ok := res["resolve "+name].([]any)
			if !ok {
				Expect(res["resolve "+name]).To(Equal("gaierror"), name)
				continue
			}
			for _, a := range addrs {
				ip, err := netip.ParseAddr(a.(string))
				Expect(err).NotTo(HaveOccurred())
				Expect(synthetic.Contains(ip)).To(BeTrue(), "%s resolved to %s", name, ip)
			}
		}
	}, NodeTimeout(2*time.Minute))

	It("denies cloud metadata endpoints", func(ctx SpecContext) {
		deniedAtConnect(ctx, targets(envOr("KOINON_ESCAPE_METADATA",
			"169.254.169.254:80,169.254.169.254:443,[fd00:ec2::254]:80,169.254.170.2:80")))

		By("the metadata name resolves to no link-local address")
		res := probe(ctx, check{"id": "resolve", "op": "resolve", "name": "metadata.google.internal"})
		if addrs, ok := res["resolve"].([]any); ok {
			for _, a := range addrs {
				ip, err := netip.ParseAddr(a.(string))
				Expect(err).NotTo(HaveOccurred())
				Expect(synthetic.Contains(ip)).To(BeTrue(), "metadata.google.internal resolved to %s", ip)
			}
		} else {
			Expect(res["resolve"]).To(Equal("gaierror"))
		}

		By("an allowed binary (curl) gets no response either")
		code, _, _ := g.curl(ctx, sandbox, "GET", "http://169.254.169.254/latest/meta-data/")
		Expect(code).To(Equal("000"))
	}, NodeTimeout(2*time.Minute))

	It("holds no service-account token and cannot reach the API server", func(ctx SpecContext) {
		res := probe(ctx,
			check{"id": "sa", "op": "read", "path": "/var/run/secrets/kubernetes.io/serviceaccount/token"},
			check{"id": "sa run", "op": "read", "path": "/run/secrets/kubernetes.io/serviceaccount/token"},
			check{"id": "pod identity", "op": "read", "path": "/.openshell/pod-identity"},
			check{"id": "env", "op": "env"},
		)
		Expect(res["sa"]).To(BeElementOf("ENOENT", "EACCES"))
		Expect(res["sa run"]).To(BeElementOf("ENOENT", "EACCES"))
		// The supervisor's identity material is mounted for OpenShell, not for the agent.
		Expect(res["pod identity"]).To(Equal("EACCES"))
		Expect(res["env"]).NotTo(HaveKey(HavePrefix("KUBERNETES_")))

		By("the API server is not reachable by Service IP or name")
		deniedAtConnect(ctx, targets(envOr("KOINON_ESCAPE_API",
			"10.96.0.1:443,kubernetes.default.svc.cluster.local:443,kubernetes.default.svc:443")))
	}, NodeTimeout(2*time.Minute))

	It("holds no credential on disk or in any environment, only placeholders", func(ctx SpecContext) {
		res := probe(ctx,
			check{
				"id": "scan", "op": "scan", "roots": scanRoots(),
				"needles": []string{hex.EncodeToString([]byte(gatewayKey)), hex.EncodeToString([]byte(githubKey))},
			},
			check{"id": "env", "op": "env"},
		)
		scan := res["scan"].(map[string]any)
		GinkgoWriter.Printf("scanned %v files\n", scan["files"])
		Expect(scan["files"]).To(BeNumerically(">", 1000), "the scan read too few files to mean anything")
		Expect(scan["found"]).To(BeEmpty(), "credential values in the sandbox")

		env := res["env"].(map[string]any)
		placeholders := append([]string{reconciler.GatewayKeyEnvVar}, reconciler.GitHubTokenEnvVars...)
		for _, name := range placeholders {
			Expect(env).To(HaveKeyWithValue(name, HavePrefix("openshell:resolve:env:")), name)
		}
		for name, v := range env {
			Expect(v).NotTo(Or(ContainSubstring(gatewayKey), ContainSubstring(githubKey)), name)
			if credentialName.MatchString(name) {
				Expect(placeholders).To(ContainElement(name), "unexpected credential-like variable %s", name)
			}
		}
	}, NodeTimeout(4*time.Minute))

	It("has no ServiceAccount token mounted anywhere, projected or not", func(ctx SpecContext) {
		res := probe(ctx,
			check{"id": "mounts", "op": "mounts", "contains": []string{"serviceaccount", "kubernetes.io", "secrets"}},
			check{"id": "files", "op": "find", "roots": scanRoots(), "contains": []string{"/serviceaccount", "kubernetes.io", "/.kube", "/kubeconfig"}},
			check{"id": "tokens dir", "op": "read", "path": "/var/run/secrets/tokens"},
			check{"id": "secrets dir", "op": "read", "path": "/var/run/secrets"},
		)
		Expect(res["mounts"]).To(BeEmpty(), "mounts that look like Kubernetes secret volumes")
		Expect(res["files"]).To(BeEmpty(), "files that look like Kubernetes credentials")
		Expect(res["tokens dir"]).To(BeElementOf("ENOENT", "EACCES"))
		Expect(res["secrets dir"]).To(BeElementOf("ENOENT", "EACCES"))
	}, NodeTimeout(2*time.Minute))

	It("cannot reach the platform's services by name or Service IP", func(ctx SpecContext) {
		// Spike 8.4 and 8.5: the API server and the gateway's and agentgateway's ports. Names
		// resolve to the synthetic range, so the refusal has to come from the connect.
		deniedAtConnect(ctx, targets(envOr("KOINON_ESCAPE_INTERNAL",
			"10.96.0.1:6443,openshell.openshell.svc.cluster.local:8080,agentgateway-mgmt.agentgateway.svc.cluster.local:4001,agentgateway.agentgateway.svc.cluster.local:8080")))
	}, NodeTimeout(2*time.Minute))

	It("runs the agent without capabilities, with no_new_privs and a seccomp filter", func(ctx SpecContext) {
		res := probe(ctx,
			check{"id": "status", "op": "status"},
			check{"id": "setuid", "op": "syscall", "name": "setuid0"},
		)
		st := res["status"].(map[string]any)
		Expect(st).To(HaveKeyWithValue("CapEff", MatchRegexp(`^0+$`)))
		Expect(st).To(HaveKeyWithValue("CapPrm", MatchRegexp(`^0+$`)))
		Expect(st).To(HaveKeyWithValue("NoNewPrivs", "1"))
		Expect(st).To(HaveKeyWithValue("Seccomp", "2"), "seccomp filter mode")
		Expect(res["setuid"]).To(Equal("EPERM"))
	}, NodeTimeout(2*time.Minute))

	It("denies writes outside the allowed paths (Landlock) and the filtered syscalls (seccomp)", func(ctx SpecContext) {
		blocked := []string{"EPERM", "EACCES"}
		res := probe(ctx,
			check{"id": "etc", "op": "write", "path": "/etc/koinon-escape"},
			check{"id": "usr", "op": "write", "path": "/usr/koinon-escape"},
			check{"id": "var", "op": "write", "path": "/var/koinon-escape"},
			check{"id": "dotdot", "op": "write", "path": "/sandbox/../etc/koinon-escape"},
			check{"id": "tmp", "op": "write", "path": "/tmp/koinon-escape"},
			check{"id": "mount", "op": "syscall", "name": "mount"},
			check{"id": "unshare user", "op": "syscall", "name": "unshare_user"},
			check{"id": "unshare mount", "op": "syscall", "name": "unshare_mount"},
			check{"id": "bpf", "op": "syscall", "name": "bpf"},
			check{"id": "keyctl", "op": "syscall", "name": "keyctl"},
			check{"id": "add_key", "op": "syscall", "name": "add_key"},
			check{"id": "perf", "op": "syscall", "name": "perf_event_open"},
			check{"id": "raw packet", "op": "syscall", "name": "raw_packet"},
			check{"id": "raw inet", "op": "syscall", "name": "raw_inet"},
		)
		Expect(res["tmp"]).To(Equal("ok"), "control: /tmp is writable")
		for _, id := range []string{"etc", "usr", "var", "dotdot", "mount", "unshare user", "unshare mount", "bpf", "keyctl", "add_key", "perf"} {
			Expect(res[id]).To(BeElementOf(blocked), id)
		}
		for _, id := range []string{"raw packet", "raw inet"} {
			Expect(res[id]).To(BeElementOf("EPERM", "EACCES", "EPROTONOSUPPORT", "EAFNOSUPPORT"), id)
		}
	}, NodeTimeout(2*time.Minute))

	It("cannot attach to or read the supervisor (PID 1)", func(ctx SpecContext) {
		res := probe(ctx,
			check{"id": "ptrace", "op": "syscall", "name": "ptrace_pid1"},
			check{"id": "mem", "op": "syscall", "name": "proc1_mem"},
		)
		Expect(res["ptrace"]).To(BeElementOf("EPERM", "EACCES", "ESRCH"))
		Expect(res["mem"]).To(BeElementOf("EPERM", "EACCES", "ENOENT"))
	}, NodeTimeout(2*time.Minute))

	// Kata is a manual release gate (ADR 0045): the home lab's dev profile may run sandboxes
	// under another runtime class, and CI has no Kata at all.
	It("runs in a Kata guest, not on the host kernel", func(ctx SpecContext) {
		host := os.Getenv("KOINON_ESCAPE_HOST_KERNEL")
		if host == "" {
			Skip("KOINON_ESCAPE_HOST_KERNEL (the nodes' uname -r) is not set: Kata is a manual release gate")
		}
		res := probe(ctx, check{"id": "kernel", "op": "kernel"})
		k := res["kernel"].(map[string]any)
		if k["release"] == host {
			Skip("the sandbox runs on the host kernel " + host + ", so it does not run under Kata")
		}
		Expect(k["release"]).NotTo(Equal(host), "guest kernel differs from the host's")
		Expect(k["hypervisor"]).To(BeTrue(), "the guest sees a hypervisor")
	}, NodeTimeout(2*time.Minute))
})
