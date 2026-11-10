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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	modelgw "github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/testing/testenv"
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
		// A model gateway stand-in with no default (ADR 0080): a name the proxy resolves to a
		// non-loopback address (OpenShell always blocks loopback and link-local) with a server on
		// port 4000 that answers every request 418 with X-Koinon-Standin: 1 (koinon-fakemodel
		// -standin; test/fixtures/rt1/manifests/escape-gateway-standin.yaml). A name that fails
		// DNS never reaches the proxy (old issue 244), and a refused connection reaches the client
		// as a TCP failure, not a 502 (old issue 270). The gateway spec skips without it; the
		// provider record below only needs some host to bind the key to.
		gwHost = envOr("KOINON_ESCAPE_GATEWAY_HOST", "agentgateway.koinon-escape.invalid")
	)

	// probe runs checks in the sandbox and returns each check's result by id.
	probeIn := func(ctx context.Context, sandbox string, checks ...check) map[string]any {
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
	probe := func(ctx context.Context, checks ...check) map[string]any {
		GinkgoHelper()
		return probeIn(ctx, sandbox, checks...)
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

		// OpenShell binds every network rule to its binaries and checks the caller at connect():
		// the flavor's harness plus /usr/bin/curl, which sends the gateway requests below. Without
		// curl the connect is refused with EACCES before anything leaves the pod (old issue 322).
		gwBinaries := append(slices.Clone(reconciler.DefaultBinaries[store.AgentFlavorOpencode]), "/usr/bin/curl")
		key := reconciler.GatewayProfile(store.AgentFlavorOpencode,
			reconciler.Flavor{Binaries: gwBinaries},
			reconciler.Endpoint{Host: gwHost, Port: 4000}, true)
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

	It("lets the sandbox post to the gateway's model routes and nothing else on it (ADR 0069)", func(ctx SpecContext) {
		// KOINON_ESCAPE_GATEWAY_HOST names the stand-in (escape-gateway-standin.koinon-fixtures.svc.cluster.local),
		// a non-loopback host the proxy resolves with a server on port 4000 that answers 418 and
		// X-Koinon-Standin: 1.
		testenv.Require(testenv.Var("escape-gateway", "KOINON_ESCAPE_GATEWAY_HOST"))
		// The stand-in answers every request 418 with the marker header, so an allowed request is
		// recognisable by what the far end said, a denied one by the proxy's 403, and anything
		// else (curl code 000 from a DNS or connect failure, a 502 from the proxy) means the
		// request did not cross the policy and the stand-in is not what answered. Runs against a
		// real OpenShell only (KOINON_OPENSHELL_ENDPOINT=... make test-escape).
		gw := "http://" + net.JoinHostPort(gwHost, "4000")
		for _, path := range modelgw.RoutePaths() {
			code, body, header := g.curlGatewayHeaders(ctx, sandbox, "POST", gw+path)
			answered(code, body)
			Expect(code).To(Equal("418"), "POST "+path+" should pass the policy and reach the stand-in at "+gwHost+":4000 (418); 403 is a policy denial, 502 or 000 means it did not get there: "+body)
			Expect(strings.ToLower(header)).To(ContainSubstring(strings.ToLower(standinMarker)), "POST "+path+": a 418 without the stand-in's marker header did not come from it")
		}
		denied := func(method, path string) {
			GinkgoHelper()
			code, body := g.curlGateway(ctx, sandbox, method, gw+path)
			answered(code, body)
			Expect(code).To(Equal("403"), method+" "+path)
			Expect(body).To(ContainSubstring("policy"), method+" "+path)
		}
		denied("GET", "/v1/messages")
		denied("POST", "/v1/messages/count_tokens")
		denied("POST", "/v1/messages/batches")
		denied("POST", "/v1/responses")
		denied("GET", "/v1/models")
		denied("POST", "/config_dump")

		By("a binary the gateway profile does not list (the probe's python3) is refused at connect")
		res := probe(ctx, tcp(target{gwHost, 4000}))
		Expect(res["tcp "+target{gwHost, 4000}.String()]).To(Equal("EACCES"),
			"the gateway rule is bound to the flavor's binaries and curl; python3 must not reach "+gwHost+":4000")
	}, NodeTimeout(2*time.Minute))

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

	It("has no SPIFFE or SPIRE agent socket reachable (ADR 0097 decision 11)", func(ctx SpecContext) {
		res := probe(ctx,
			check{"id": "mounts", "op": "mounts", "contains": []string{"spire", "spiffe"}},
			check{"id": "files", "op": "find", "roots": scanRoots(), "contains": []string{"spire", "spiffe", "agent.sock"}},
			check{"id": "run", "op": "read", "path": "/run/spire"},
			check{"id": "var run", "op": "read", "path": "/var/run/spire"},
			check{"id": "tmp", "op": "read", "path": "/tmp/spire-agent"},
		)
		Expect(res["mounts"]).To(BeEmpty(), "mounts that look like the SPIRE agent socket or the SPIFFE CSI driver")
		files := res["files"].(map[string]any)
		Expect(files["visited"]).To(BeNumerically(">", 1000), "the walk read too few entries to mean anything")
		Expect(files["found"]).To(BeEmpty(), "files that look like a SPIFFE socket")
		Expect(res["run"]).To(BeElementOf("ENOENT", "EACCES"))
		Expect(res["var run"]).To(BeElementOf("ENOENT", "EACCES"))
		Expect(res["tmp"]).To(BeElementOf("ENOENT", "EACCES"))
	}, NodeTimeout(4*time.Minute))

	It("has no ServiceAccount token mounted anywhere, projected or not", func(ctx SpecContext) {
		res := probe(ctx,
			check{"id": "mounts", "op": "mounts", "contains": []string{"serviceaccount", "kubernetes.io", "secrets"}},
			check{"id": "files", "op": "find", "roots": scanRoots(), "contains": []string{"/serviceaccount", "kubernetes.io", "/.kube", "/kubeconfig"}},
			check{"id": "tokens dir", "op": "read", "path": "/var/run/secrets/tokens"},
			check{"id": "secrets dir", "op": "read", "path": "/var/run/secrets"},
		)
		Expect(res["mounts"]).To(BeEmpty(), "mounts that look like Kubernetes secret volumes")
		files := res["files"].(map[string]any)
		Expect(files["visited"]).To(BeNumerically(">", 1000), "the walk read too few entries to mean anything")
		Expect(files["found"]).To(BeEmpty(), "files that look like Kubernetes credentials")
		Expect(res["tokens dir"]).To(BeElementOf("ENOENT", "EACCES"))
		Expect(res["secrets dir"]).To(BeElementOf("ENOENT", "EACCES"))
	}, NodeTimeout(2*time.Minute))

	It("cannot reach the platform's services by name or Service IP", func(ctx SpecContext) {
		// Spike 8.4 and 8.5: the API server and the gateway's and agentgateway's ports. Names
		// resolve to the synthetic range, so the refusal has to come from the connect.
		deniedAtConnect(ctx, targets(envOr("KOINON_ESCAPE_INTERNAL",
			"10.96.0.1:6443,openshell.openshell.svc.cluster.local:8080,agentgateway-mgmt.agentgateway.svc.cluster.local:4001,agentgateway.agentgateway.svc.cluster.local:8080")))
	}, NodeTimeout(2*time.Minute))

	It("cannot reach the platform Services' ClusterIPs", func(ctx SpecContext) {
		// The Services are looked up with the cluster client (see admission_live_test.go);
		// KOINON_ESCAPE_SERVICES is a comma-separated namespace/name:port list.
		testenv.Require(testenv.Var("cluster", "KOINON_ESCAPE_NAMESPACE")) // and KOINON_ESCAPE_KUBECONFIG, to look up Service IPs
		cs := kubeClient()
		var ts []target
		for _, s := range strings.Split(envOr("KOINON_ESCAPE_SERVICES", "openshell/openshell:8080,agentgateway/agentgateway-mgmt:4001,agentgateway/agentgateway:8080"), ",") {
			nsName, port, _ := strings.Cut(strings.TrimSpace(s), ":")
			ns, name, _ := strings.Cut(nsName, "/")
			svc, err := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				// The e2e lists the install's own Services, which must exist (KOINON_ESCAPE_POLICY_REQUIRED=1).
				Expect(os.Getenv("KOINON_ESCAPE_POLICY_REQUIRED")).NotTo(Equal("1"), "listed platform Service %s cannot be read: %v", nsName, err)
				GinkgoWriter.Printf("service %s: %v\n", nsName, err)
				continue
			}
			p, err := strconv.Atoi(port)
			Expect(err).NotTo(HaveOccurred(), s)
			ts = append(ts, target{svc.Spec.ClusterIP, p})
		}
		if len(ts) == 0 {
			Skip("none of the platform Services could be read with the cluster client")
		}
		deniedAtConnect(ctx, ts)
	}, NodeTimeout(2*time.Minute))

	It("runs on pods of the shape the sandbox admission policy selects and expects (#109)", func(ctx SpecContext) {
		// The admission specs dry-run copies; this looks at the pods OpenShell and the Agent Sandbox
		// controller really made for the sandbox above.
		testenv.Require(testenv.Var("cluster", "KOINON_ESCAPE_NAMESPACE"))
		realPods(ctx, kubeClient(), os.Getenv("KOINON_ESCAPE_NAMESPACE"))
	}, NodeTimeout(time.Minute))

	It("cannot reach another sandbox in the workspace", func(ctx SpecContext) {
		// Spike 8.6. The second sandbox listens on 8099; its pod IP is what it reports for its
		// own host name.
		other := fmt.Sprintf("kesb-%d", n)
		g.sandbox(ctx, execution.SandboxSpec{
			Name: other, Policy: reconciler.SandboxPolicy(nil),
			Command: []string{"python3", "-m", "http.server", "8099", "--bind", "0.0.0.0"},
		})
		// The command starts with the sandbox, so its listener can come up after the sandbox reports
		// ready: wait for it, or the isolation check below would be skipped by a lost race (old issue 323).
		Eventually(func() any {
			return probeIn(ctx, other, check{"id": "local", "op": "tcp", "host": "127.0.0.1", "port": 8099})["local"]
		}).WithContext(ctx).WithTimeout(90*time.Second).WithPolling(2*time.Second).
			Should(Equal("open"), "control: the second sandbox's listener is up")
		out, stderr, code := g.exec(ctx, other, "", "python3", "-c", "import socket;print(socket.gethostbyname(socket.gethostname()))")
		Expect(code).To(Equal(0), stderr)
		ip := strings.TrimSpace(out)
		Expect(netip.ParseAddr(ip)).NotTo(BeZero(), "the second sandbox's address: %q", out)
		Expect(netip.MustParseAddr(ip).IsLoopback()).To(BeFalse(), "a pod IP, not loopback")
		deniedAtConnect(ctx, []target{{ip, 8099}})
	}, NodeTimeout(8*time.Minute))

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

	// The writes go to places file permissions allow (sticky, world-writable directories), so
	// only Landlock's read-write list (/tmp, /dev/null, the workdir) stops them. The syscalls
	// and raw sockets fail for want of capabilities as well as by seccomp; the spec above
	// checks both are in force, and this one checks the operations really fail.
	It("denies writes outside Landlock's read-write list, and privileged syscalls", func(ctx SpecContext) {
		res := probe(ctx,
			check{"id": "shm", "op": "write", "path": "/dev/shm/koinon-escape"},
			check{"id": "vartmp", "op": "write", "path": "/var/tmp/koinon-escape"},
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
		for _, id := range []string{"shm", "vartmp"} {
			Expect(res[id]).To(BeElementOf("EACCES", "EPERM"), id)
		}
		for _, id := range []string{"mount", "unshare user", "unshare mount", "bpf", "keyctl", "add_key", "perf"} {
			Expect(res[id]).To(BeElementOf("EPERM", "EACCES"), id)
		}
		for _, id := range []string{"raw packet", "raw inet"} {
			Expect(res[id]).To(BeElementOf("EPERM", "EACCES", "EPROTONOSUPPORT", "EAFNOSUPPORT"), id)
		}
	}, NodeTimeout(2*time.Minute))

	It("cannot attach to or read the supervisor (PID 1)", func(ctx SpecContext) {
		res := probe(ctx,
			check{"id": "ptrace", "op": "syscall", "name": "ptrace_pid1"},
			check{"id": "mem", "op": "syscall", "name": "proc1_mem"},
			check{"id": "visible", "op": "read", "path": "/proc/1/status"},
		)
		Expect(res["visible"]).To(Equal("ok"), "control: PID 1 is visible, so ESRCH would not mean it is hidden")
		Expect(res["ptrace"]).To(BeElementOf("EPERM", "EACCES"))
		Expect(res["mem"]).To(BeElementOf("EPERM", "EACCES", "ENOENT"))
	}, NodeTimeout(2*time.Minute))

	// Kata is a manual release gate (ADR 0045): a maintainer cluster's dev profile may run sandboxes
	// under another runtime class, and CI has no Kata at all.
	It("runs in a Kata guest, not on the host kernel", func(ctx SpecContext) {
		// KOINON_ESCAPE_HOST_KERNEL is the nodes' uname -r; set it to require a Kata guest.
		testenv.Require(testenv.Var("kata", "KOINON_ESCAPE_HOST_KERNEL"))
		host := os.Getenv("KOINON_ESCAPE_HOST_KERNEL")
		res := probe(ctx, check{"id": "kernel", "op": "kernel"})
		k := res["kernel"].(map[string]any)
		Expect(k["release"]).NotTo(Equal(host), "the sandbox runs on the host kernel, so not under Kata")
		Expect(k["hypervisor"]).To(BeTrue(), "the guest sees a hypervisor")
	}, NodeTimeout(2*time.Minute))
})
