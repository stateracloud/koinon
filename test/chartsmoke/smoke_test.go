// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/network"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// Deadlines from the brief: a Job finishes, and a Deployment's probes answer, within a minute.
const (
	jobDeadline   = 60 * time.Second
	probeDeadline = 60 * time.Second
)

// dbSecretPassword is obviously fake, like every secret in the tests.
const dbSecretPassword = "koinon_test_not-a-real-password"

// thirdPartyDeployments are rendered Deployments the smoke test does not run: they are not
// Koinon's binaries, so no Koinon flag or mount can break them. koinon-gateway is agentgateway,
// whose configuration the reconciler writes at run time.
var thirdPartyDeployments = []string{"koinon-gateway"}

var _ = Describe("Rendered startup", Label("chartsmoke"), func() {
	for _, vf := range valuesFiles() {
		name := strings.TrimSuffix(filepath.Base(vf), "-values.yaml")
		if mode := postgresMode(vf); mode != "external" {
			// cnpg: the components' URIs use sslmode=verify-full against the CNPG cluster's CA and
			// the host koinon-pg-rw.<ns>.svc, with the password in PGPASSWORD. A Postgres container
			// with a CA of its own would test the TLS set-up, not the chart's startup contract,
			// and the Koinon containers' flags, env and mounts are otherwise the same as in
			// external mode (the same templates), which are covered.
			It(name+" (postgres.mode="+mode+") is not run: its database needs CNPG's CA", func() {
				Skip("covered by the external-database modes: same components, flags and mounts; only the DATABASE_URL differs")
			})
			continue
		}
		describeMode(name, vf)
	}
})

func describeMode(name, valuesFile string) {
	Describe(name, Ordered, func() {
		var (
			r  *rendered
			cl *cluster
			rn *runner
			// ready records the Deployments whose containers answered their probes.
			ready = map[string]*workload{}
			// accounted collects the Deployments the specs ran or deliberately skipped.
			accounted []string
		)

		BeforeAll(func(ctx SpecContext) {
			r = helmRender(ctx, valuesFile)

			nw, err := network.New(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx context.Context) { _ = nw.Remove(ctx) })
			pg, err := postgres.Run(ctx, pgtest.Image,
				postgres.WithDatabase("koinon"), postgres.WithUsername("koinon"), postgres.WithPassword(dbSecretPassword),
				postgres.BasicWaitStrategies(), network.WithNetwork([]string{"postgres"}, nw))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx context.Context) { _ = testcontainers.TerminateContainer(pg) })

			cl = startCluster()
			DeferCleanup(cl.Stop)

			tmp, err := os.MkdirTemp("", "koinon-chartsmoke-")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = os.RemoveAll(tmp) })
			rn = &runner{cl: cl, nwName: nw.Name, images: images, tmp: tmp}
			DeferCleanup(rn.Close)

			applyChartObjects(ctx, cl, r)
			provisionDatabaseSecret(ctx, cl, r)
		}, NodeTimeout(5*time.Minute))

		AfterEach(func(ctx SpecContext) {
			if CurrentSpecReport().Failed() {
				for _, w := range ready {
					AddReportEntry("log "+w.Name, w.Logs(ctx))
				}
			}
		})

		// The pre-install hook Jobs, in hook-weight order, the way Helm runs them: ensure-secrets
		// creates the generated Secrets, then the migration applies the schema.
		It("runs the ensure-secrets hook Job to completion and creates the Secrets", func(ctx SpecContext) {
			job := r.job("koinon-ensure-secrets")
			Expect(job).NotTo(BeNil(), "the values file renders the ensure-secrets Job")
			runJob(ctx, rn, job)
			// What the chart expects from it.
			Expect(cl.Secret(ctx, "koinon-otlp-token")).NotTo(BeNil())
			Expect(cl.Secret(ctx, "koinon-otlp-token").Data).To(HaveKey("token"))
		})

		It("runs the migrate Job to completion", func(ctx SpecContext) {
			job := r.job("koinon-migrate")
			Expect(job).NotTo(BeNil())
			runJob(ctx, rn, job)
		})

		It("runs any other Job to completion", func(ctx SpecContext) {
			for _, j := range r.Jobs {
				if !slices.Contains([]string{"koinon-ensure-secrets", "koinon-migrate"}, j.Name) {
					runJob(ctx, rn, j)
				}
			}
		})

		It("starts koinon-api and answers its probes", func(ctx SpecContext) {
			ready["koinon-api"] = startDeployment(ctx, rn, r, "koinon-api")
			accounted = append(accounted, "koinon-api")
			// The KEK Secret is created by the API on its first start (KOINON_KEK_CREATE).
			Expect(cl.Secret(ctx, "koinon-kek")).NotTo(BeNil(), "koinon-api creates the key-encryption Secret")
		})

		It("starts koinon-reconciler and answers its probes", func(ctx SpecContext) {
			ready["koinon-reconciler"] = startDeployment(ctx, rn, r, "koinon-reconciler")
			accounted = append(accounted, "koinon-reconciler")
		})

		It("starts koinon-agentd and answers its probes", func(ctx SpecContext) {
			ready["koinon-agentd"] = startDeployment(ctx, rn, r, "koinon-agentd")
			accounted = append(accounted, "koinon-agentd")
		})

		It("accounts for every rendered Deployment", func() {
			var all, skipped []string
			for _, d := range r.Deployments {
				all = append(all, d.Name)
				if slices.Contains(thirdPartyDeployments, d.Name) {
					accounted = append(accounted, d.Name)
					skipped = append(skipped, d.Name)
				}
			}
			sort.Strings(all)
			sort.Strings(accounted)
			// A Deployment added to the chart must be started here or declared third-party.
			Expect(slices.Compact(accounted)).To(Equal(all), "run new Deployments in this suite, or add them to thirdPartyDeployments")
			AddReportEntry("not run (third-party)", fmt.Sprint(skipped))
			AddReportEntry("not applied (not needed to start)", fmt.Sprint(r.Ignored))
		})
	})
}

// platformJobs are Jobs that inspect the cluster platform (Agent Sandbox, OpenShell, Kata, the
// gateway controller), which envtest does not have. They must run their checks and report: exit 1
// with the report is the expected outcome here, a crash before the report (a flag or config error)
// or a permission error is not.
var platformJobs = map[string]struct{ report string }{
	"koinon-preflight": {report: "Koinon install preflight"},
}

// runJob runs every container of the Job's pod template to completion and requires exit code 0
// (for a platform Job: its report, and no Forbidden answer from the API server, which would mean
// the chart's RBAC lacks a rule).
func runJob(ctx context.Context, rn *runner, job *batchv1.Job) {
	GinkgoHelper()
	ps := job.Spec.Template.Spec
	for _, c := range append(slices.Clone(ps.InitContainers), ps.Containers...) {
		w := rn.start(ctx, job.Name, ps, c, jobDeadline)
		code, logs := w.ExitCode(ctx), w.Logs(ctx)
		AddReportEntry("log "+w.Name, logs, ReportEntryVisibilityFailureOrVerbose)
		if p, ok := platformJobs[job.Name]; ok {
			Expect(logs).To(ContainSubstring(p.report), "%s did not reach its checks (exit %d, args %q)", w.Name, code, c.Args)
			Expect(logs).NotTo(ContainSubstring("forbidden"), "%s was denied by the API server: the chart's RBAC lacks a rule", w.Name)
			Expect(code).To(BeElementOf(0, 1), "%s exited with code %d", w.Name, code)
			continue
		}
		Expect(code).To(Equal(0), "%s exited with code %d (args %q); logs:\n%s", w.Name, code, c.Args, logs)
	}
}

// startDeployment starts each container of the Deployment's pod template and requires its
// probes (startup, readiness, liveness) to answer within a minute. A container without HTTP
// probes must serve /metrics on its "metrics" port instead, which the chart scrapes.
func startDeployment(ctx context.Context, rn *runner, r *rendered, name string) *workload {
	GinkgoHelper()
	d := r.deployment(name)
	Expect(d).NotTo(BeNil(), "%s is rendered", name)
	ps := d.Spec.Template.Spec
	var first *workload
	for _, c := range ps.Containers {
		w := rn.start(ctx, name, ps, c, 0)
		if first == nil {
			first = w
		}
		probes := []*corev1.Probe{c.StartupProbe, c.ReadinessProbe, c.LivenessProbe}
		any := false
		for _, p := range probes {
			if p == nil || p.HTTPGet == nil {
				continue
			}
			any = true
			Expect(w.ProbeHTTP(ctx, p, probeDeadline)).To(BeEmpty(), "%s probe; logs:\n%s", w.Name, w.Logs(ctx))
		}
		if !any {
			Expect(slices.IndexFunc(c.Ports, func(p corev1.ContainerPort) bool { return p.Name == "metrics" })).To(BeNumerically(">=", 0),
				"%s has no HTTP probe and no metrics port to check", w.Name)
			probe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/metrics", Port: intstr.FromString("metrics")}}}
			Expect(w.ProbeHTTP(ctx, probe, probeDeadline)).To(BeEmpty(), "%s /metrics; logs:\n%s", w.Name, w.Logs(ctx))
		}
		Expect(w.Running(ctx)).To(BeTrue(), "%s is still running; logs:\n%s", w.Name, w.Logs(ctx))
	}
	return first
}

// applyChartObjects creates what the chart renders besides the workloads: the namespace,
// ServiceAccounts, RBAC, ConfigMaps and Secrets, in the API server. Hook annotations do not
// matter here; the specs run the hook Jobs in the order Helm would.
func applyChartObjects(ctx context.Context, cl *cluster, r *rendered) {
	GinkgoHelper()
	c := cl.Admin
	_, err := c.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	create := func(err error) {
		GinkgoHelper()
		Expect(err).NotTo(HaveOccurred())
	}
	for _, o := range r.ServiceAcct {
		_, err := c.CoreV1().ServiceAccounts(namespace).Create(ctx, o, metav1.CreateOptions{})
		create(err)
	}
	for _, o := range r.ConfigMaps {
		_, err := c.CoreV1().ConfigMaps(namespace).Create(ctx, o, metav1.CreateOptions{})
		create(err)
	}
	for _, o := range r.Secrets {
		_, err := c.CoreV1().Secrets(namespace).Create(ctx, o, metav1.CreateOptions{})
		create(err)
	}
	for _, o := range r.Roles {
		_, err := c.RbacV1().Roles(namespace).Create(ctx, o, metav1.CreateOptions{})
		create(err)
	}
	for _, o := range r.RoleBindings {
		_, err := c.RbacV1().RoleBindings(namespace).Create(ctx, o, metav1.CreateOptions{})
		create(err)
	}
	for _, o := range r.ClusterRoles {
		_, err := c.RbacV1().ClusterRoles().Create(ctx, o, metav1.CreateOptions{})
		create(err)
	}
	for _, o := range r.CRBindings {
		_, err := c.RbacV1().ClusterRoleBindings().Create(ctx, o, metav1.CreateOptions{})
		create(err)
	}
}

// provisionDatabaseSecret creates the Secret the operator provides in external mode
// (postgres.external.secretName): one libpq URI per role, here all the same role, for the
// Postgres container on the network. The Secret and keys are whatever the rendered containers
// read DATABASE_URL from.
func provisionDatabaseSecret(ctx context.Context, cl *cluster, r *rendered) {
	GinkgoHelper()
	dsn := fmt.Sprintf("postgres://koinon:%s@postgres:5432/koinon?sslmode=disable", dbSecretPassword)
	secrets := map[string]map[string]string{}
	visit := func(ps corev1.PodSpec) {
		for _, c := range append(slices.Clone(ps.InitContainers), ps.Containers...) {
			for _, e := range c.Env {
				if e.Name == "DATABASE_URL" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
					ref := e.ValueFrom.SecretKeyRef
					if secrets[ref.Name] == nil {
						secrets[ref.Name] = map[string]string{}
					}
					secrets[ref.Name][ref.Key] = dsn
				}
			}
		}
	}
	for _, j := range r.Jobs {
		visit(j.Spec.Template.Spec)
	}
	for _, d := range r.Deployments {
		visit(d.Spec.Template.Spec)
	}
	Expect(secrets).NotTo(BeEmpty(), "external mode reads DATABASE_URL from a Secret")
	for name, kv := range secrets {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name}, StringData: kv}
		_, err := cl.Admin.CoreV1().Secrets(namespace).Create(ctx, s, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}
}
