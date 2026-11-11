// SPDX-License-Identifier: Apache-2.0

package drill_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	tokenv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/test/drill"
	"github.com/stateracloud/koinon/test/e2e/e2eclient"
)

// side is one Koinon install the drill talks to.
type side struct {
	in      drill.Install
	gw      drill.Gateway
	org     string
	agent   string
	token   string // an admin token, once made
	client  *e2eclient.Client
	stopFwd func()
}

func (s *side) name(component string) string { return s.in.Release + "-" + component }

var (
	src, dst side
	// done records which steps finished, so a check after a failed step says so instead of failing in a confusing way.
	done = map[string]bool{}
	// enqueued are the workflows the source had ENQUEUED when the backup began.
	enqueued []string
	// baseline is the source gateway's objects when the source was declared lost.
	baseline []drill.Object
	// markers records when each RPO marker was written: days value -> time.
	markers sync.Map
	// dstBefore are the dst's counters when the held window began.
	fakeAtRelease int
	// sourceSession is the source admin's session cookie, for the quarantine check.
	sourceSession string
	snapshotUnix  int64 // when the backup began: a fire due after it is not in the dump
	releasedUnix  int64
)

const retentionMin = 30 // run-content-retention days: the marker is retentionMin+n

func needs(steps ...string) {
	GinkgoHelper()
	for _, s := range steps {
		Expect(done[s]).To(BeTrue(), "an earlier step did not finish: %s", s)
	}
}

// step names a step of the drill in the report; check names come from the label.
func step(name string, body func(ctx SpecContext)) {
	id := strings.TrimSuffix(strings.Fields(name)[0], ":")
	It(name, Label("step", id), func(ctx SpecContext) {
		body(ctx)
		done[id] = true
	}, NodeTimeout(20*time.Minute))
}

func must(err error, what string) {
	GinkgoHelper()
	Expect(err).NotTo(HaveOccurred(), what)
}

func readFile(path string) string {
	GinkgoHelper()
	b, err := os.ReadFile(path)
	must(err, path)
	return string(b)
}

// cleanups run once, from the AfterAll of the Ordered container. DeferCleanup inside an It would run
// after that spec (Ginkgo makes it a cleanup-after-each), tearing the install down after its own step.
var cleanups []func(context.Context)

func onSuiteEnd(f func(context.Context)) { cleanups = append(cleanups, f) }

// ensureNamespace creates a namespace carrying the run label, and removes it at the end of the suite.
func ensureNamespace(ctx context.Context, ns string) {
	GinkgoHelper()
	must(drill.CheckNamespace(ns), ns)
	_, err := kube.KubectlStdin(ctx, fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n  labels:\n    %s: \"%s\"\n", ns, drill.RunLabel, runID), "apply", "-f", "-")
	must(err, "create namespace "+ns)
	onSuiteEnd(func(ctx context.Context) {
		if os.Getenv("KOINON_DRILL_KEEP") == "1" {
			return
		}
		// Only a namespace that still carries this run's label is deleted.
		_, _ = kube.Kubectl(ctx, "delete", "namespace", "-l", drill.RunLabel+"="+runID, "--field-selector", "metadata.name="+ns, "--wait=false")
	})
}

func installGateway(ctx context.Context, in drill.Install) drill.Gateway {
	GinkgoHelper()
	ensureNamespace(ctx, in.GatewayNamespace)
	_, err := kube.Helm(ctx, "upgrade", "--install", in.GatewayRelease, top.GatewayChart, "--version", top.GatewayVersion,
		// The OpenShell chart has no top-level gatewayId; its gateway_id comes from server.sandboxJwt.gatewayId (default: the release name).
		"--namespace", in.GatewayNamespace, "-f", filepath.Join(repoRoot, top.GatewayValues), "--set", "server.sandboxJwt.gatewayId="+in.GatewayID, "--wait", "--timeout", "8m")
	must(err, "install the OpenShell gateway "+in.GatewayRelease)
	onSuiteEnd(func(ctx context.Context) {
		if os.Getenv("KOINON_DRILL_KEEP") != "1" {
			_, _ = kube.Helm(ctx, "uninstall", in.GatewayRelease, "--namespace", in.GatewayNamespace, "--wait=false")
		}
	})
	gw, err := drill.DiscoverGateway(ctx, kube, in.GatewayNamespace)
	must(err, "find the gateway of "+in.GatewayNamespace)
	return gw
}

// installKoinon installs the chart under test into a fresh namespace with the install's own gateway.
func installKoinon(ctx context.Context, s *side, hold bool) {
	GinkgoHelper()
	in := s.in
	ensureNamespace(ctx, in.Namespace)
	// The gateway's client certificate, copied into the install's namespace (the quickstart's client-cert block).
	raw, err := kube.Kubectl(ctx, "-n", s.gw.Namespace, "get", "secret", s.gw.ClientTLSSecret, "-o", "json")
	must(err, "read the gateway client certificate")
	var sec map[string]any
	must(json.Unmarshal([]byte(raw), &sec), "parse the client certificate Secret")
	sec["metadata"] = map[string]any{"name": s.gw.ClientTLSSecret, "namespace": in.Namespace, "labels": map[string]string{drill.RunLabel: runID}}
	b, err := json.Marshal(sec)
	must(err, "marshal")
	_, err = kube.KubectlStdin(ctx, string(b), "apply", "-f", "-")
	must(err, "copy the client certificate")

	overlay, err := drill.ValuesOverlay(top.FakesNamespace, s.gw, s.in.GatewayID, hold, top.StorageClass)
	must(err, "values overlay")
	overlayFile := filepath.Join(workDir, in.Release+"-values.yaml")
	must(os.WriteFile(overlayFile, []byte(overlay), 0o600), "write values")
	args := []string{
		"install", in.Release, filepath.Join(repoRoot, top.ChartPath), "--namespace", in.Namespace,
		"-f", filepath.Join(repoRoot, top.ValuesFile), "-f", overlayFile, "--timeout", "12m",
	}
	if !hold {
		args = append(args, "--wait")
	}
	_, err = kube.Helm(ctx, args...)
	must(err, "helm install "+in.Release)
	if hold {
		// A held reconciler converges nothing (ADR 0107), so it never makes the gateway's ConfigMap and the gateway
		// Deployment cannot become ready until the hold is released: `helm --wait` would wait for it for ever.
		// What a held install must reach is its three binaries, ready. The hooks (preflight, migrate) ran above.
		for _, c := range []string{"api", "reconciler", "agentd"} {
			out, err := kube.Kubectl(ctx, "-n", in.Namespace, "rollout", "status", "deploy/"+s.name(c), "--timeout=10m")
			must(err, "the held "+c+" becomes ready: "+out)
		}
	}
	onSuiteEnd(func(ctx context.Context) {
		if os.Getenv("KOINON_DRILL_KEEP") != "1" {
			_, _ = kube.Helm(ctx, "uninstall", in.Release, "--namespace", in.Namespace, "--wait=false")
		}
	})
	// Label what outlives a namespace or is cluster-scoped, so the sweep finds it.
	for _, suffix := range []string{"install-mark", "kek", "bootstrap-admin"} {
		_, _ = kube.Kubectl(ctx, "-n", in.Namespace, "label", "--overwrite", "secret", in.Release+"-"+suffix, drill.RunLabel+"="+runID)
	}
	_, _ = kube.Kubectl(ctx, "label", "--overwrite", "clusterrole,clusterrolebinding,validatingadmissionpolicy,validatingadmissionpolicybinding",
		"-l", "app.kubernetes.io/instance="+in.Release, drill.RunLabel+"="+runID)
}

// openAPI port-forwards the install's API and returns a client for it.
func openAPI(ctx context.Context, s *side) {
	GinkgoHelper()
	if s.stopFwd != nil {
		s.stopFwd()
	}
	addr, stop, err := kube.PortForward(context.WithoutCancel(ctx), s.in.Namespace, "svc/"+s.name("api"), 8080)
	must(err, "port-forward the API of "+s.in.Release)
	s.stopFwd = stop
	s.client = e2eclient.New("http://" + addr)
}

func orgOf(ctx context.Context, c *e2eclient.Client) string {
	GinkgoHelper()
	me, err := c.Auth.WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
	must(err, "who am I")
	for _, m := range me.GetMemberships() {
		if m.GetOrgSlug() == "default" {
			return m.GetOrgId()
		}
	}
	Fail("the user has no membership in the default org")
	return ""
}

func admin(ctx context.Context, s *side, method, path string, body ...string) (int, []byte) {
	GinkgoHelper()
	code, b, err := s.client.Admin(ctx, s.token, method, path, body...)
	must(err, method+" "+path)
	return code, b
}

func sql(ctx context.Context, s *side, q string) string {
	GinkgoHelper()
	out, err := kube.SQL(ctx, s.in.Namespace, s.in.Release, q)
	must(err, q)
	return out
}

func sqlInt(ctx context.Context, s *side, q string) int {
	GinkgoHelper()
	n, err := strconv.Atoi(sql(ctx, s, q))
	must(err, q)
	return n
}

// fakeRequests reads the fake model's request log through a short port-forward.
func fakeRequests(ctx context.Context) []e2eclient.FakeModelRequest {
	GinkgoHelper()
	addr, stop, err := kube.PortForward(ctx, top.FakesNamespace, "svc/fakemodel", 8080)
	must(err, "port-forward the fake model")
	defer stop()
	reqs, err := e2eclient.FakeModelRequests(ctx, "http://"+addr)
	must(err, "read the fake model's requests")
	return reqs
}

// metric reads one gauge from a component's /metrics through a port-forward to its Deployment.
func metric(ctx context.Context, s *side, component, name string) float64 {
	GinkgoHelper()
	addr, stop, err := kube.PortForward(ctx, s.in.Namespace, "deploy/"+s.name(component), 9090)
	must(err, "port-forward "+component+" metrics")
	defer stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/metrics", nil)
	must(err, "request")
	rsp, err := http.DefaultClient.Do(req)
	must(err, "scrape "+component)
	defer func() { _ = rsp.Body.Close() }()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := rsp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	for _, line := range strings.Split(sb.String(), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == name {
			v, err := strconv.ParseFloat(f[1], 64)
			must(err, line)
			return v
		}
	}
	Fail(fmt.Sprintf("%s does not export %s", component, name))
	return -1
}

func secretValue(ctx context.Context, ns, name, key string) string {
	GinkgoHelper()
	out, err := kube.Kubectl(ctx, "-n", ns, "get", "secret", name, "-o", "jsonpath={.data."+key+"}")
	must(err, "read secret "+name)
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	must(err, "decode secret "+name)
	return string(b)
}

// knownGap runs a probe of behaviour a product issue will fix. A finding fails the spec with
// "known gap: #N"; no finding means the gap is closed, which the report says so the marker can go.
func knownGap(issue, what string, probe func() (finding string)) {
	GinkgoHelper()
	if f := probe(); f != "" {
		Fail(drill.GapMessage(issue, what+": "+f))
	}
	AddReportEntry("known-gap-closed", fmt.Sprintf("%s (%s) passes: remove the known-gap label from this spec", issue, what))
}

var _ = Describe("the restore drill", Ordered, ContinueOnFailure, func() {
	marks := &res.Marks

	AfterAll(func(ctx SpecContext) {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i](ctx)
		}
	}, NodeTimeout(15*time.Minute))

	step("fakes: the drill's fake model server", func(ctx SpecContext) {
		ensureNamespace(ctx, top.FakesNamespace)
		script := readFile(filepath.Join(repoRoot, "test", "drill", "fakemodel", "drill.yaml"))
		_, err := kube.KubectlStdin(ctx, drill.FakeModelManifest(top.FakesNamespace, runID, top.FakeModelImage, script), "apply", "-f", "-")
		must(err, "apply the fake model")
		_, err = kube.Kubectl(ctx, "-n", top.FakesNamespace, "rollout", "status", "deploy/fakemodel", "--timeout=3m")
		must(err, "fake model rollout")
	})

	step("gateways: one OpenShell gateway for each install", func(ctx SpecContext) {
		src.in, dst.in = top.Source, top.Target
		src.gw = installGateway(ctx, top.Source)
		dst.gw = installGateway(ctx, top.Target)
		Expect(src.gw.Namespace).NotTo(Equal(dst.gw.Namespace))
	})

	step("source: install the Koinon under test", func(ctx SpecContext) {
		needs("gateways")
		installKoinon(ctx, &src, false)
	})

	step("seed: the source, through the supported APIs only", func(ctx SpecContext) {
		needs("source", "fakes")
		openAPI(ctx, &src)
		c := src.client

		By("the bootstrap password becomes a fixed fake one")
		boot := secretValue(ctx, src.in.Namespace, src.name("bootstrap-admin"), "password")
		changeRequired, err := c.Login(ctx, "admin", boot)
		must(err, "login with the bootstrap password")
		Expect(changeRequired).To(BeTrue())
		_, err = c.Auth.ChangePassword(ctx, &authv1alpha1.ChangePasswordRequest{CurrentPassword: boot, NewPassword: drill.SeededPassword})
		must(err, "change password")
		src.org = orgOf(ctx, c)
		sourceSession = c.SessionToken()
		tok, err := c.Tokens.CreateToken(ctx, &tokenv1alpha1.CreateTokenRequest{OrgId: src.org, Name: "drill", Role: authv1alpha1.OrgRole_ORG_ROLE_ADMIN})
		must(err, "create a token")
		src.token = tok.GetSecret()

		By("an API provider with the fake key, on the fake model")
		code, body := admin(ctx, &src, http.MethodPost, "/providers",
			fmt.Sprintf(`{"name":"drillfake","type":"openai","url":%q,"apiKey":%q}`, drill.FakeModelURL(top.FakesNamespace), drill.SeededProviderKey))
		Expect(code).To(Equal(http.StatusCreated), string(body))
		var prov struct{ ID string }
		must(json.Unmarshal(body, &prov), "provider")
		code, body = admin(ctx, &src, http.MethodPost, "/models", fmt.Sprintf(`{"providerId":%q,"name":%q}`, prov.ID, drill.ModelName))
		Expect(code).To(Equal(http.StatusCreated), string(body))
		code, body = admin(ctx, &src, http.MethodPost, "/prices",
			fmt.Sprintf(`{"name":"fake-drill","provider":"openai","match":{"glob":%q},"prices":{"input":"0.30","output":"1.20"},"maxOutput":4096}`, drill.ModelName))
		Expect(code).To(Equal(http.StatusCreated), string(body))
		code, body = admin(ctx, &src, http.MethodPut, "/default-model", fmt.Sprintf(`{"model":%q}`, drill.ModelName))
		Expect(code).To(Equal(http.StatusOK), string(body))

		By("an agent with a * * * * * schedule")
		ts, err := c.Templates.ListTemplates(ctx, &templatev1alpha1.ListTemplatesRequest{})
		must(err, "templates")
		tmpl := ""
		for _, t := range ts.GetTemplates() {
			if t.GetSlug() == "web-researcher" { // no grants, so it needs no connection
				tmpl = t.GetId()
			}
		}
		Expect(tmpl).NotTo(BeEmpty(), "the web-researcher template")
		created, err := c.Agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
			OrgId: src.org, TemplateId: tmpl, DisplayName: "Drill agent", Flavor: templatev1alpha1.Flavor_FLAVOR_OPENCODE,
		})
		must(err, "create the agent")
		src.agent = created.GetAgent().GetId()
		withTrigger, err := c.Agents.SetAgentTriggers(ctx, &agentv1alpha1.SetAgentTriggersRequest{
			OrgId: src.org, AgentId: src.agent, BaseVersion: created.GetAgent().GetLatestVersion(),
			Triggers: []*templatev1alpha1.Trigger{{
				Id: drill.TriggerID, Type: templatev1alpha1.TriggerType_TRIGGER_TYPE_SCHEDULE, Cron: "* * * * *", Timezone: "UTC",
				Instruction: "Answer with one short sentence.",
			}},
		})
		must(err, "set the trigger")
		_, err = c.Agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{
			OrgId: src.org, AgentId: src.agent, Version: withTrigger.GetAgent().GetLatestVersion(),
		})
		must(err, "activate the agent")

		By("the source ran once: a scheduled run reached the fake model and succeeded, so the source has a sandbox")
		Eventually(func() int { return sqlInt(ctx, &src, "SELECT count(*) FROM runs WHERE state = 'succeeded'") }).
			WithContext(ctx).WithTimeout(top.ReleasedWait).WithPolling(10 * time.Second).Should(BeNumerically(">=", 1))

		By("low org trigger limits, then Run now N times: N-1 stay ENQUEUED")
		out, err := kube.Exec(ctx, src.in.Namespace, "deploy/"+src.name("api"), "koinon-api", "org-limits", "set", "--org", "default",
			"--trigger-concurrency", "1", "--trigger-rate-per-minute", "1")
		must(err, "org-limits set: "+out)
		// Run now goes through the org's queue, so with the low limits only the first call is accepted at once.
		// The others wait for the rate limit, and the request gives up after 30 s with "queue is busy"
		// (connect.CodeUnavailable); the admin API shows that as 500 "internal error", not 503. The workflow
		// stays ENQUEUED, and that is what the drill needs. The calls go out a second apart (see below) and wait together, so the wait is one
		// timeout, not N. What the check holds to is the end state: at least one 202, no other status than a
		// 202, 500 or 503, and N-1 workflows ENQUEUED when the backup starts.
		// TODO(#529): the 500 is a product gap. Exit condition: when #529 maps CodeUnavailable to 503,
		// stop accepting 500 here. Do not let it hide a real failure by accepting any other status.
		type reply struct {
			code int
			body string
		}
		replies := make([]reply, top.RunNowCalls)
		var calls sync.WaitGroup
		for i := range replies {
			calls.Add(1)
			go func() {
				defer GinkgoRecover()
				defer calls.Done()
				// The workflow ID ends in the unix second, so calls in one second are one workflow.
				time.Sleep(time.Duration(i) * 1100 * time.Millisecond)
				code, body := admin(ctx, &src, http.MethodPost, "/agents/"+src.agent+"/triggers/"+drill.TriggerID+"/run")
				replies[i] = reply{code, string(body)}
			}()
		}
		calls.Wait()
		accepted := 0
		for _, r := range replies {
			switch r.code {
			case http.StatusAccepted:
				accepted++
			case http.StatusInternalServerError, http.StatusServiceUnavailable:
			default:
				Fail(fmt.Sprintf("Run now answered %d: %s", r.code, r.body))
			}
		}
		Expect(accepted).To(BeNumerically(">=", 1), "no Run now call was accepted: %v", replies)
		AddReportEntry("run-now replies", fmt.Sprintf("%v", replies))
	})

	step("backup: the guide's backup blocks, then the source is lost", func(ctx SpecContext) {
		needs("seed")
		// The RPO marker writer: a recorded setting change every 2 seconds, until the disaster.
		stopMarkers := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stopMarkers:
					return
				case <-time.After(2 * time.Second):
				}
				days := retentionMin + n
				code, _, err := src.client.Admin(ctx, src.token, http.MethodPut, "/run-content-retention", fmt.Sprintf(`{"days":%d}`, days))
				if err == nil && code == http.StatusOK {
					markers.Store(days, time.Now())
				}
			}
		}()
		Eventually(func() bool { _, ok := markers.Load(retentionMin + 1); return ok }).WithTimeout(time.Minute).Should(BeTrue(), "the marker writer works")

		// The snapshot: what is ENQUEUED now is what the dump will hold, and a trigger fire due after this
		// instant is not in it. Both are read just before the backup-db block, not at the end of the seed.
		snapshotUnix = time.Now().Unix()
		rows := sql(ctx, &src, "SELECT workflow_uuid FROM dbos.workflow_status WHERE status = 'ENQUEUED' ORDER BY created_at")
		enqueued = strings.Fields(rows)
		Expect(len(enqueued)).To(BeNumerically(">=", top.RunNowCalls-1), "N-1 Run-now calls stay ENQUEUED behind the low limits, got %v", enqueued)
		AddReportEntry("enqueued at backup", enqueued)

		marks.DumpStart = time.Now()
		for _, b := range []string{"backup-keys", "check-keys", "backup-db", "verify-dump"} {
			out, err := kube.GuideBlock(ctx, repoRoot, workDir, b, src.in.Namespace, src.in.Release)
			must(err, out)
		}
		marks.DumpEnd = time.Now()
		Expect(filepath.Join(workDir, "koinon.dump")).To(BeARegularFile())

		// t_disaster: the source is lost. Its components stop; its gateway and its objects stay, for the isolation check.
		close(stopMarkers)
		wg.Wait()
		marks.Disaster = time.Now()
		_, err := kube.Kubectl(ctx, "-n", src.in.Namespace, "scale", "deployment/"+src.name("agentd"), "deployment/"+src.name("reconciler"), "deployment/"+src.name("api"), "--replicas=0")
		must(err, "scale the source to zero")
		_, err = kube.Kubectl(ctx, "-n", src.in.Namespace, "wait", "--for=delete", "pod", "-l", "app.kubernetes.io/instance="+src.in.Release+",app.kubernetes.io/component in (api,reconciler,agentd)", "--timeout=5m")
		must(err, "the source pods stop")
		if src.stopFwd != nil {
			src.stopFwd()
			src.stopFwd = nil
		}

		var err2 error
		baseline, err2 = drill.SnapshotGateway(ctx, kube, src.gw, os.Getenv("KOINON_DRILL_OPENSHELL_SERVERNAME"))
		must(err2, "snapshot the source gateway")
		kinds := map[string]int{}
		for _, o := range baseline {
			kinds[o.Kind]++
		}
		Expect(kinds["sandbox"]).To(BeNumerically(">=", 1), "the seed left no sandbox on the source gateway: %v", kinds)
		Expect(kinds["provider"]).To(BeNumerically(">=", 1), "the seed left no provider on the source gateway: %v", kinds)
		AddReportEntry("source gateway objects", kinds)
	})

	step("target: a fresh namespace, its own gateway, held", func(ctx SpecContext) {
		needs("backup")
		installKoinon(ctx, &dst, true)
		marks.TargetInstalled = time.Now()
	})

	step("restore: the guide's restore blocks", func(ctx SpecContext) {
		needs("target")
		// The export names the Secret after the source release; the target's release is another, because both
		// installs share this cluster (and its cluster-scoped chart objects). A real restore into a fresh
		// namespace keeps the release name and has no such step.
		file := filepath.Join(workDir, "koinon-kek-backup.yaml")
		renamed, err := drill.RenameKEKSecret(readFile(file), src.name("kek"), dst.name("kek"))
		must(err, "rename the KEK Secret in the export")
		must(os.WriteFile(file, []byte(renamed), 0o600), "write the export")
		// The guide's resync block needs the checkpoint bucket; phase 1 runs with checkpoints off, so it
		// has no snapshots to list and the block is left for phase 2 (ADR 0109).
		for _, b := range []string{"stop", "restore-db", "restore-keys", "start"} {
			out, err := kube.GuideBlock(ctx, repoRoot, workDir, b, dst.in.Namespace, dst.in.Release)
			must(err, out)
		}
		marks.Restored = time.Now()
	})

	Describe("held", func() {
		It("is held on all three binaries and does nothing for a schedule slot", Label("hold"), func(ctx SpecContext) {
			needs("restore")
			for _, c := range []string{"api", "reconciler", "agentd"} {
				Expect(metric(ctx, &dst, c, "koinon_restore_hold")).To(BeEquivalentTo(1), "%s is not held", c)
			}
			snap := func() string {
				return strings.Join([]string{
					"tasks=" + sql(ctx, &dst, "SELECT count(*) FROM tasks"),
					"claimed=" + sql(ctx, &dst, "SELECT count(*) FROM runs WHERE owner_id IS NOT NULL AND state IN ('starting', 'running') AND lease_expires_at > now()"),
					"dbos=" + strings.ReplaceAll(sql(ctx, &dst, "SELECT status || ':' || count(*) FROM dbos.workflow_status GROUP BY status ORDER BY 1"), "\n", ","),
					"model-requests=" + strconv.Itoa(len(fakeRequests(ctx))),
				}, " ")
			}
			before := snap()
			AddReportEntry("held, before", before)
			time.Sleep(top.HeldObserve) // spans at least one slot of the every-minute schedule
			after := snap()
			AddReportEntry("held, after", after)
			Expect(after).To(Equal(before), "something acted while the install was held")
			Expect(sqlInt(ctx, &dst, "SELECT count(*) FROM runs WHERE owner_id IS NOT NULL AND state IN ('starting', 'running') AND lease_expires_at > now()")).To(BeZero())
			for _, c := range []string{"api", "reconciler", "agentd"} {
				Expect(metric(ctx, &dst, c, "koinon_restore_hold")).To(BeEquivalentTo(1), "%s released itself", c)
			}
		}, NodeTimeout(10*time.Minute))

		It("opens the restored keys", Label("keys"), func(ctx SpecContext) {
			needs("restore")
			out, err := kube.GuideBlock(ctx, repoRoot, workDir, "verify-keys", dst.in.Namespace, dst.in.Release)
			must(err, out)
		})

		It("has its own install mark", Label("mark"), func(ctx SpecContext) {
			needs("restore")
			a := secretValue(ctx, src.in.Namespace, src.name("install-mark"), "token")
			b := secretValue(ctx, dst.in.Namespace, dst.name("install-mark"), "token")
			Expect(a).NotTo(BeEmpty())
			Expect(b).NotTo(BeEmpty())
			Expect(b).NotTo(Equal(a), "the target must not own the source's OpenShell objects (ADR 0106)")
		})

		It("only reports what its importer finds while held", Label("known-gap", "importer"), func(ctx SpecContext) {
			needs("restore")
			// A declared provider Secret appears in the target's namespace while it is held.
			secret := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: drill-declared
  namespace: %s
  labels:
    %s/secret-type: model-provider
    %s/org: default
    %s: "%s"
stringData:
  name: drill-declared
  provider: openai
  url: %s
  apiKey: koinon_test_not-a-real-key
  models: |
    - name: drill
`, dst.in.Namespace, project.Domain, project.Domain, drill.RunLabel, runID, drill.FakeModelURL(top.FakesNamespace))
			_, err := kube.KubectlStdin(ctx, secret, "apply", "-f", "-")
			must(err, "declare a provider")
			time.Sleep(60 * time.Second)
			dst.token = src.token // the restored admin token reads the held install
			openAPI(ctx, &dst)
			code, body := admin(ctx, &dst, http.MethodGet, "/providers")
			Expect(code).To(Equal(http.StatusOK), string(body))
			knownGap("#516", "the importer only reports while the install is held", func() string {
				if strings.Contains(string(body), "drill-declared") {
					return "the importer created the declared provider while the install was held"
				}
				return ""
			})
		}, NodeTimeout(5*time.Minute))

		It("can disable its MCP servers", Label("known-gap", "mcp-disable"), func(ctx SpecContext) {
			needs("restore")
			knownGap("#516", "a held copy can disable the MCP servers it restored", func() string {
				return "there is no disable flag for MCP servers on a held install"
			})
		})
	})

	step("release: the guide's release block", func(ctx SpecContext) {
		needs("restore")
		marks.HeldChecksDone = time.Now()
		reqs := fakeRequests(ctx)
		fakeAtRelease = len(reqs)
		releasedUnix = time.Now().Unix()
		out, err := kube.GuideBlock(ctx, repoRoot, workDir, "release", dst.in.Namespace, dst.in.Release)
		must(err, out)
		marks.Released = time.Now()
		releasedUnix = marks.Released.Unix()
		for _, c := range []string{"api", "reconciler", "agentd"} {
			Expect(metric(ctx, &dst, c, "koinon_restore_hold")).To(BeEquivalentTo(0), "%s is still held after the release", c)
		}
	})

	Describe("released", func() {
		It("signs in with the seeded password", Label("signin"), func(ctx SpecContext) {
			needs("release")
			openAPI(ctx, &dst)
			Eventually(func() error {
				c := e2eclient.New(dst.client.Base)
				change, err := c.Login(ctx, "admin", drill.SeededPassword)
				if err == nil && change {
					return fmt.Errorf("the restored admin must not need a password change")
				}
				if err == nil {
					dst.client = c
				}
				return err
			}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
			dst.org = orgOf(ctx, dst.client)
			tok, err := dst.client.Tokens.CreateToken(ctx, &tokenv1alpha1.CreateTokenRequest{OrgId: dst.org, Name: "drill-dst", Role: authv1alpha1.OrgRole_ORG_ROLE_ADMIN})
			must(err, "create a token on the target")
			dst.token = tok.GetSecret()
		})

		It("sends the seeded provider key to the fake model", Label("provider-key"), func(ctx SpecContext) {
			needs("release")
			Eventually(func(g Gomega) {
				reqs := fakeRequests(ctx)
				g.Expect(len(reqs)).To(BeNumerically(">", fakeAtRelease), "no run has reached the fake model since the release")
				found := false
				for _, r := range reqs[fakeAtRelease:] {
					if strings.Contains(r.Headers.Get("Authorization"), drill.SeededProviderKey) {
						found = true
					}
				}
				g.Expect(found).To(BeTrue(), "no request carries the seeded key in its Authorization header")
			}).WithContext(ctx).WithTimeout(top.ReleasedWait).WithPolling(10 * time.Second).Should(Succeed())
			marks.FirstRunDone = time.Now()
		}, NodeTimeout(15*time.Minute))

		It("fires one catch-up per due trigger, then a normal fire", Label("schedule"), func(ctx SpecContext) {
			needs("release")
			// A fire is the DBOS workflow "trigger-fire:<org>:<trigger>@<due unix time>". Fires with a due
			// time after the snapshot and up to the release are catch-up fires; the dump holds the earlier ones.
			dues := func() (catchUp, normal int, err error) {
				rows, err := kube.SQL(ctx, dst.in.Namespace, dst.in.Release, "SELECT workflow_uuid FROM dbos.workflow_status WHERE workflow_uuid LIKE 'trigger-fire:%'")
				if err != nil {
					return 0, 0, err
				}
				for _, id := range strings.Fields(rows) {
					i := strings.LastIndex(id, "@")
					if i < 0 {
						continue
					}
					due, err := strconv.ParseInt(id[i+1:], 10, 64)
					if err != nil {
						continue
					}
					switch {
					case due > snapshotUnix && due <= releasedUnix:
						catchUp++
					case due > releasedUnix:
						normal++
					}
				}
				return catchUp, normal, nil
			}
			Eventually(func(g Gomega) {
				c, n, err := dues()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(c).To(Equal(1), "exactly one catch-up fire after the release")
				g.Expect(n).To(BeNumerically(">=", 1), "then a normal fire")
			}).WithContext(ctx).WithTimeout(5 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())
			c, _, err := dues()
			must(err, "read the fires")
			Expect(c).To(Equal(1), "the catch-up must not repeat")
		}, NodeTimeout(10*time.Minute))

		It("completes every run that was ENQUEUED, once", Label("workflow-queue"), func(ctx SpecContext) {
			needs("release")
			Expect(enqueued).NotTo(BeEmpty())
			// A workflow's run is Koinon's own row: dedupe_records maps the fire key of the workflow ID to the
			// task it made, and runs.task_id to the run. dbos.workflow_status is keyed by workflow_uuid, so
			// counting it by that key proves nothing about doing the work once.
			runs := func(id string) (total, terminal int, err error) {
				key, ok := fireKey(id)
				if !ok {
					return 0, 0, fmt.Errorf("workflow ID %q is not a trigger fire", id)
				}
				q := fmt.Sprintf("SELECT count(*) || ' ' || count(*) FILTER (WHERE r.state IN ('succeeded','failed','canceled')) FROM runs r JOIN dedupe_records d ON d.org_id = r.org_id AND d.effect_kind = 'task' AND d.effect_id = r.task_id WHERE d.key = '%s'", strings.ReplaceAll(key, "'", "''"))
				out, err := kube.SQL(ctx, dst.in.Namespace, dst.in.Release, q)
				if err != nil {
					return 0, 0, err
				}
				if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d %d", &total, &terminal); err != nil {
					return 0, 0, fmt.Errorf("read %q: %w", out, err)
				}
				return total, terminal, nil
			}
			Eventually(func(g Gomega) {
				for _, id := range enqueued {
					status, err := kube.SQL(ctx, dst.in.Namespace, dst.in.Release, fmt.Sprintf("SELECT status FROM dbos.workflow_status WHERE workflow_uuid = '%s'", strings.ReplaceAll(id, "'", "''")))
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(status).To(Equal("SUCCESS"), "workflow %s", id)
					total, terminal, err := runs(id)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(terminal).To(Equal(1), "workflow %s: runs in a terminal state (of %d)", id, total)
				}
			}).WithContext(ctx).WithTimeout(2 * top.ReleasedWait).WithPolling(15 * time.Second).Should(Succeed())
			for _, id := range enqueued {
				total, terminal, err := runs(id)
				must(err, "count the runs of "+id)
				Expect(total).To(Equal(1), "workflow %s made exactly one run", id)
				Expect(terminal).To(Equal(1), "workflow %s: its run is terminal", id)
			}
			marks.QueueDrained = time.Now()
		}, NodeTimeout(30*time.Minute))

		It("keeps the source's sandboxes and providers as they were", Label("isolation"), func(ctx SpecContext) {
			needs("backup", "release")
			// Self-check: the cleanups run at the end of the container, so the gateway is still there.
			_, err := kube.Kubectl(ctx, "get", "namespace", src.gw.Namespace)
			must(err, "the source gateway's namespace is gone before the isolation check: cleanup ran too early")
			after, err := drill.SnapshotGateway(ctx, kube, src.gw, os.Getenv("KOINON_DRILL_OPENSHELL_SERVERNAME"))
			must(err, "snapshot the source gateway again")
			Expect(drill.DiffObjects(baseline, after)).To(BeEmpty(), "the target changed objects of the source's gateway")
		})

		It("refuses a session token of the source", Label("known-gap", "quarantine"), func(ctx SpecContext) {
			needs("release")
			knownGap("#516", "a session of the source install is refused on the restored one", func() string {
				c := e2eclient.New(dst.client.Base)
				c.SetSession(sourceSession)
				if _, err := c.Auth.WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{}); err == nil {
					return "the source's session cookie is accepted by the target"
				}
				return ""
			})
		})
	})

	step("finish: the data lost (RPO) and the time taken (RTO)", func(ctx SpecContext) {
		needs("release")
		// The newest marker the target holds: the retention days the writer last set that the dump had.
		code, body := admin(ctx, &dst, http.MethodGet, "/run-content-retention")
		Expect(code).To(Equal(http.StatusOK), string(body))
		var rc struct{ Days int }
		must(json.Unmarshal(body, &rc), "retention")
		if t, ok := markers.Load(rc.Days); ok {
			marks.LatestMarker = t.(time.Time)
		} else {
			Fail(fmt.Sprintf("the target's retention is %d days: not a marker the writer set, so the RPO cannot be measured", rc.Days))
		}
		marks.AllChecksPassed = time.Now()
		t := marks.Compute()
		AddReportEntry("RTO seconds", t.RTOSeconds)
		AddReportEntry("RPO seconds", t.RPOSeconds)
		AddReportEntry("phases (seconds)", t.Phases)
	})
})

// fireKey is the dedupe key of the trigger fire a workflow ID names: "run-now:<org>:<trigger>:<unix>" and
// "trigger-fire:<org>:<trigger>@<unix>" both fire the key "<trigger>@<unix>" (durable.TriggerFireKey).
func fireKey(workflowID string) (string, bool) {
	parts := strings.Split(workflowID, ":")
	switch {
	case parts[0] == "run-now" && len(parts) == 4:
		return parts[2] + "@" + parts[3], true
	case parts[0] == "trigger-fire" && len(parts) == 3:
		return parts[2], true
	}
	return "", false
}
