// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/network"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/stateracloud/koinon/internal/guestauth"
	"github.com/stateracloud/koinon/internal/imagecacheca"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// Deadlines from the brief: a Job finishes, and a Deployment's probes answer, within a minute.
const (
	jobDeadline   = 60 * time.Second
	probeDeadline = 60 * time.Second
	// settleTime is how long the Deployments run after they are ready: a component that dies
	// shortly after its first probe, or whose RBAC is missing a rule it only needs after
	// starting, shows up in this window.
	settleTime = 12 * time.Second
)

// thirdPartyDeployments are rendered Deployments the smoke test does not run: they are not
// Koinon's binaries, so no Koinon flag or mount can break them. koinon-gateway is agentgateway,
// whose configuration the reconciler writes at run time. koinon-imagecache is Zot, the Substrate
// image cache (issue #1170); the chart-check assertions and the substrate spec cover its render.
var thirdPartyDeployments = []string{"koinon-gateway", "koinon-imagecache"}

// platformJobs are Jobs that inspect the cluster platform (Agent Sandbox, OpenShell, Kata, the
// gateway controller), which envtest does not have. They must run their checks and report: exit 1
// with the report is the expected outcome here, a crash before the report (a flag or config error)
// or a permission error is not.
var platformJobs = map[string]struct{ report string }{
	"koinon-preflight": {report: "Koinon install preflight"},
}

var _ = Describe("Rendered startup", Label("chartsmoke"), func() {
	for _, vf := range selectedValuesFiles() {
		describeMode(strings.TrimSuffix(filepath.Base(vf), "-values.yaml"), vf)
	}
})

func describeMode(name, valuesFile string) {
	Describe(name, Ordered, func() {
		var (
			r  *rendered
			cl *cluster
			rn *runner
			// ready records the workloads of the Deployments that answered their probes.
			ready = map[string][]*workload{}
			// accounted collects the Deployments the specs ran or deliberately skipped.
			accounted []string
			// dbURL reaches the Postgres container from the test process.
			dbURL string
		)

		BeforeAll(func(ctx SpecContext) {
			r = helmRender(ctx, valuesFile)

			nw, err := network.New(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx context.Context) { _ = nw.Remove(ctx) })
			pg, err := postgres.Run(ctx, pgtest.Image,
				postgres.WithDatabase("koinon"), postgres.WithUsername("koinon"), postgres.WithPassword(dbSecretPassword),
				postgres.BasicWaitStrategies(), network.WithNetwork([]string{"postgres"}, nw))
			if pg != nil {
				DeferCleanup(func(ctx context.Context) { terminate(pg) })
			}
			Expect(err).NotTo(HaveOccurred())
			dbURL, err = pg.ConnectionString(ctx, "sslmode=disable")
			Expect(err).NotTo(HaveOccurred())

			bootstrapDatabase(ctx, dbURL)

			cl = startCluster()

			docker, err := testcontainers.NewDockerClientWithOpts(ctx)
			Expect(err).NotTo(HaveOccurred())
			rn = &runner{cl: cl, nwName: nw.Name, images: images, docker: docker, helperImage: pgtest.Image}
			DeferCleanup(rn.Close)

			// What exists before the install: the namespace, its default ServiceAccount (made by
			// the cluster's controller) and the operator's Secrets.
			_, err = cl.Admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = cl.Admin.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			AddReportEntry("operator Secrets", fmt.Sprint(provisionOperatorSecrets(ctx, cl, r)))
			// The KMS the KEKs are wrapped by, set up as the operator would before the install.
			if k := kmsOf(r); k != nil {
				k.start(ctx, cl, nw.Name)
			}
		}, NodeTimeout(5*time.Minute))

		AfterEach(func(ctx SpecContext) {
			if CurrentSpecReport().Failed() {
				for _, ws := range ready {
					for _, w := range ws {
						AddReportEntry("log "+w.Name, w.Logs(ctx))
					}
				}
			}
		})

		// Helm runs the pre-install hooks before it creates any other object of the release, in
		// ascending hook-weight order; Jobs of one weight finish before the next weight starts.
		// So a hook Job can only use what the operator made before the install and the hook
		// objects of its own or an earlier weight: ensure-secrets creates the generated Secrets,
		// then the migration applies the schema.
		It("runs the pre-install hooks in hook-weight order", func(ctx SpecContext) {
			hooks := map[int][]runtime.Object{}
			var weights []int
			add := func(o runtime.Object) {
				m := mustMeta(o)
				if m.GetAnnotations()["helm.sh/hook"] == "pre-upgrade" {
					return // an upgrade-only hook: it has its own spec below
				}
				if w, pre, hook := hookOf(m); hook {
					Expect(pre).To(BeTrue(), "%s %s is a hook of a kind this suite does not run: %s", kindOf(o), m.GetName(), m.GetAnnotations()["helm.sh/hook"])
					if _, ok := hooks[w]; !ok {
						weights = append(weights, w)
					}
					hooks[w] = append(hooks[w], o)
				}
			}
			for _, o := range r.Objects {
				add(o)
			}
			seeded := map[string]bool{}
			for _, o := range r.Foreign {
				if _, _, hook := hookOf(mustMeta(o)); hook {
					// The CA hook Job (issue #1191) acts on the Substrate namespace. Make what the installer leaves
					// there before helm runs: the namespace and the atelet DaemonSet with the installer patch, once.
					if ns := mustMeta(o).GetNamespace(); !seeded[ns] {
						seeded[ns] = true
						seedSubstrateNamespace(ctx, cl, ns)
					}
				}
				add(o)
			}
			for _, j := range r.Jobs {
				add(j)
			}
			sort.Ints(weights)

			var ran []string
			for _, w := range weights {
				// Helm creates the non-Job objects of a weight with the Jobs; the Jobs' pods start
				// once the API server has them all.
				var jobs []*batchv1.Job
				for _, o := range hooks[w] {
					if j, ok := o.(*batchv1.Job); ok {
						jobs = append(jobs, j)
						continue
					}
					Expect(applyObject(ctx, cl, o)).To(Succeed())
				}
				sort.Slice(jobs, func(i, k int) bool { return jobs[i].Name < jobs[k].Name })
				for _, j := range jobs {
					By(fmt.Sprintf("hook weight %d: %s", w, j.Name))
					runJob(ctx, rn, j)
					ran = append(ran, j.Name)
					if j.Name == "koinon-migrate" {
						// CNPG reconciles its managed roles on its own schedule, after the migration
						// may have run: the login roles appear here, never before.
						createComponentRoles(ctx, dbURL)
					}
				}
			}
			AddReportEntry("hook Jobs, in order", fmt.Sprint(ran))
			for ns := range seeded {
				// The CA hook Job ran for real: it published the public CA and stamped atelet.
				cm, err := cl.Admin.CoreV1().ConfigMaps(ns).Get(ctx, imagecacheca.ConfigMapName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(cm.Data).To(HaveKey(imagecacheca.CAKey))
				Expect(cm.Data).NotTo(HaveKey("tls.key"))
				ds, err := cl.Admin.AppsV1().DaemonSets(ns).Get(ctx, imagecacheca.DaemonSetName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(ds.Spec.Template.Annotations).To(HaveKey(imagecacheca.AteletAnnotation))
			}
			Expect(ran).To(ContainElement("koinon-migrate"), "the migrate Job is a pre-install hook")
			if slices.Contains(ran, "koinon-ensure-secrets") {
				// What the chart expects from it: each token Secret it generates (a Secret the operator
				// supplies, as in external-operator-secrets mode, is not the Job's to create). The
				// reconciler and agentd read the install mark from its Secret (issue 461).
				generated := r.generatedSecretNames()
				for _, name := range []string{"koinon-otlp-token", "koinon-otlp-traces-token", "koinon-install-mark"} {
					if generated[name] {
						Expect(cl.Secret(ctx, name)).NotTo(BeNil(), name)
						Expect(cl.Secret(ctx, name).Data).To(HaveKey("token"), name)
					}
				}
				// With the substrate driver: the key that signs the guest API tokens (issue #1309), a hex
				// Ed25519 seed under "key" that the reconciler and agentd read at start.
				if generated["koinon-substrate-guest-key"] {
					Expect(cl.Secret(ctx, "koinon-substrate-guest-key")).NotTo(BeNil())
					_, err := guestauth.ParseSeed(string(cl.Secret(ctx, "koinon-substrate-guest-key").Data["key"]))
					Expect(err).NotTo(HaveOccurred())
				}
			}
		}, SpecTimeout(5*time.Minute))

		// The rest of the release: everything that is not a hook, then its Jobs that are not hooks.
		It("creates the release's objects and runs its plain Jobs", func(ctx SpecContext) {
			for _, o := range r.Objects {
				if _, _, hook := hookOf(mustMeta(o)); !hook {
					Expect(applyObject(ctx, cl, o)).To(Succeed())
				}
			}
			// The Deployments exist in the API server too (nothing runs their pods there; the
			// containers are started below): the reconciler makes its ConfigMap owned by the
			// gateway Deployment, so that Deployment has to be readable.
			for _, d := range r.Deployments {
				Expect(applyObject(ctx, cl, d)).To(Succeed())
			}
			for _, j := range r.Jobs {
				if _, _, hook := hookOf(&j.ObjectMeta); !hook {
					if j.Labels["app.kubernetes.io/component"] == "run-evidence" {
						// An operator-run Job (ADR 0121, #966): it exports one recorded run to S3,
						// so it needs a run, a KEK Secret and a bucket this suite does not make.
						// TestChartContract checks its render; the export code has its own tests.
						continue
					}
					runJob(ctx, rn, j)
				}
			}
		}, SpecTimeout(5*time.Minute))

		// The restore release check (ADR 0110 decision 1, issue 516) is a pre-upgrade hook: it starts koinon-api
		// as "restore release-check" with the migration role's database URL, the install mark Secret (optional)
		// and POD_NAMESPACE. Here it runs against the migrated database of a plain install: a first release has
		// no pending release, so it must exit 0 and print the report. (No Kubernetes API is reachable from the
		// runner, so the importer plan is left out; the importer's own specs cover the plan.)
		It("runs the restore release check against the migrated database and passes", func(ctx SpecContext) {
			var job *batchv1.Job
			for _, j := range r.Jobs {
				if strings.HasSuffix(j.Name, "-restore-release-check") {
					job = j
				}
			}
			if strings.HasSuffix(name, "restore-hold") {
				// A held install renders no release check: the hold is the check (ADR 0110 decision 1).
				Expect(job).To(BeNil(), "a held install renders no restore-release-check Job")
				return
			}
			Expect(job).NotTo(BeNil(), "the chart renders the restore-release-check Job while restoreHold is off")
			Expect(job.Annotations).To(HaveKeyWithValue("helm.sh/hook", "pre-upgrade"))
			Expect(job.Spec.Template.Spec.Containers[0].Args).To(Equal([]string{"restore", "release-check"}))
			// The Job's ServiceAccount, Role and RoleBinding are pre-upgrade hooks of lower weight: Helm
			// creates them before the Job, and the install spec above skipped them.
			for _, o := range r.Objects {
				if mustMeta(o).GetAnnotations()["helm.sh/hook"] == "pre-upgrade" {
					Expect(applyObject(ctx, cl, o)).To(Succeed(), "the check's ServiceAccount, Role or RoleBinding")
				}
			}
			runJob(ctx, rn, job)
		}, SpecTimeout(3*time.Minute))

		// With a KMS (ADR 0085 decision 12) the key-admin Job, a plain Job of the release, created the
		// KEK Secret and added the KEK keyAdmin.addKey names, each wrapped by the KMS: the Secret
		// holds no key in the clear, and koinon-api, which may only decrypt, starts from it below.
		// Each KEK is bound (format 2) to the install ID the migration wrote into the database
		// (old issue 615), which the Job reads with the database settings the chart gives it, and the Job
		// set the bound-only marker of the new install.
		It("wraps every KEK with the KMS, bound to the install", func(ctx SpecContext) {
			k := kmsOf(r)
			if k == nil {
				Skip("the KEKs are local in this mode")
			}
			conn, err := pgx.Connect(ctx, dbURL)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(conn.Close)
			var install string
			Expect(conn.QueryRow(ctx, "SELECT install_id::text FROM install_identity").Scan(&install)).To(Succeed())
			// A new install is bound only from its first KEK on: the Job set the marker before it
			// created the KEK Secret, so no process ever accepts an unbound KMS KEK here.
			var setBy string
			Expect(conn.QueryRow(ctx, "SELECT set_by FROM kek_bound_only").Scan(&setBy)).To(Succeed(), "the key-admin Job set the bound-only marker")
			Expect(setBy).To(Equal("first-install"))
			s := cl.Secret(ctx, kekSecretName(r))
			Expect(s).NotTo(BeNil(), "the key-admin Job created the KEK Secret")
			Expect(s.Data).To(HaveKey("active"))
			var ids []string
			for id, v := range s.Data {
				if id == "active" {
					continue
				}
				ids = append(ids, id)
				var stored struct {
					Fmt          int
					Backend, Key string
					Install      string
				}
				Expect(json.Unmarshal(v, &stored)).To(Succeed(), "KEK %s is stored wrapped (JSON), not as the key", id)
				Expect(stored.Backend).To(Equal(k.backend), "KEK %s", id)
				Expect(stored.Key).To(Equal(k.keyName()), "KEK %s", id)
				Expect(stored.Fmt).To(Equal(2), "KEK %s is bound to the install", id)
				Expect(stored.Install).To(Equal(install), "KEK %s is bound to this database's install ID", id)
			}
			recovery := false
			for _, j := range r.Jobs {
				for _, c := range j.Spec.Template.Spec.Containers {
					for _, a := range c.Args {
						if id, ok := strings.CutPrefix(a, "--add-key="); ok {
							Expect(ids).To(ContainElement(id), "the key-admin Job added %s", id)
							Expect(string(s.Data["active"])).NotTo(Equal(id), "the added KEK is inactive: the Secret had a working active KEK")
						}
						recovery = recovery || a == "--activate-if-no-active"
					}
				}
			}
			// The values file sets keyAdmin.activateIfNoActive (#255). The Job exited 0 above, so the
			// binary parsed the flag the chart passes; with a working active KEK it changed nothing.
			Expect(recovery).To(BeTrue(), "the key-admin Job is rendered with --activate-if-no-active")
			Expect(ids).To(ContainElement(string(s.Data["active"])))
		})

		// The start-up check scopes its generated-Secret list to this Helm release (#375), so two
		// releases in one namespace do not see each other's ensure-secrets Secrets.
		It("gives koinon-api the Helm release name to scope generated Secrets", func() {
			api := r.deployment("koinon-api")
			Expect(api).NotTo(BeNil())
			args := api.Spec.Template.Spec.Containers[0].Args
			i := slices.Index(args, "--release")
			Expect(i).To(BeNumerically(">=", 0), "koinon-api has --release")
			Expect(i + 1).To(BeNumerically("<", len(args)))
			Expect(args[i+1]).To(Equal(release), "the chart passes .Release.Name")
		})

		// koinon-api makes the audit checkpoint signing key on its first checkpoint and is the only
		// reader (ADR 0100 decision 4.2, #334): the chart gives it the Secret's name, renders no
		// Secret of that name (an upgrade would overwrite the key) and no other workload mounts or
		// reads it.
		It("gives koinon-api the audit signing Secret name and no other workload a reference to it", func() {
			api := r.deployment("koinon-api")
			Expect(api).NotTo(BeNil())
			args := api.Spec.Template.Spec.Containers[0].Args
			i := slices.Index(args, "--audit-signing-secret")
			Expect(i).To(BeNumerically(">=", 0), "koinon-api has --audit-signing-secret")
			Expect(i + 1).To(BeNumerically("<", len(args)))
			name := args[i+1]
			Expect(name).To(HaveSuffix("-audit-signing"))

			refers := func(spec corev1.PodSpec) bool {
				for _, v := range spec.Volumes {
					if (v.Secret != nil && v.Secret.SecretName == name) || v.Projected != nil && slices.ContainsFunc(v.Projected.Sources, func(p corev1.VolumeProjection) bool {
						return p.Secret != nil && p.Secret.Name == name
					}) {
						return true
					}
				}
				for _, c := range append(slices.Clone(spec.InitContainers), spec.Containers...) {
					if slices.Contains(c.Args, name) || slices.Contains(c.Command, name) {
						return true
					}
					for _, e := range c.Env {
						if e.Value == name || (e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == name) {
							return true
						}
					}
					for _, ef := range c.EnvFrom {
						if ef.SecretRef != nil && ef.SecretRef.Name == name {
							return true
						}
					}
				}
				return false
			}
			for _, d := range r.Deployments {
				if d.Name == "koinon-api" {
					Expect(refers(d.Spec.Template.Spec)).To(BeTrue(), "koinon-api names the Secret")
					continue
				}
				Expect(refers(d.Spec.Template.Spec)).To(BeFalse(), "%s must not reference the audit signing Secret", d.Name)
			}
			for _, j := range r.Jobs {
				Expect(refers(j.Spec.Template.Spec)).To(BeFalse(), "Job %s must not reference the audit signing Secret", j.Name)
			}
			for _, o := range r.Objects {
				if sec, ok := o.(*corev1.Secret); ok {
					Expect(sec.Name).NotTo(Equal(name), "the chart must not render the audit signing Secret")
				}
			}
		})

		// With the DBOS runtime on (ADR 0085), the executor ID is the pod name from the downward API,
		// and the migrate Job (a pre-install hook, so it ran above) creates the tables the runtime
		// needs before koinon-api starts.
		It("gives a durable koinon-api its pod name as the executor ID", func() {
			api := r.deployment("koinon-api")
			Expect(api).NotTo(BeNil())
			c := api.Spec.Template.Spec.Containers[0]
			if !slices.Contains(c.Args, "--durable") {
				Skip("api.durable is off in this mode")
			}
			var ref *corev1.EnvVar
			for i := range c.Env {
				if c.Env[i].Name == "KOINON_EXECUTOR_ID" {
					ref = &c.Env[i]
				}
			}
			Expect(ref).NotTo(BeNil(), "koinon-api has KOINON_EXECUTOR_ID")
			Expect(ref.ValueFrom).NotTo(BeNil())
			Expect(ref.ValueFrom.FieldRef).NotTo(BeNil())
			Expect(ref.ValueFrom.FieldRef.FieldPath).To(Equal("metadata.name"))
			for _, j := range r.Jobs {
				if j.Name != "koinon-migrate" {
					continue
				}
				_, pre, hook := hookOf(j)
				Expect(hook && pre).To(BeTrue(), "the migrate Job runs before koinon-api exists")
				Expect(j.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--durable"), "the migrate Job creates the DBOS tables")
			}
		})

		It("starts koinon-api and answers its probes", func(ctx SpecContext) {
			ready["koinon-api"] = startDeployment(ctx, rn, r, "koinon-api")
			accounted = append(accounted, "koinon-api")
			// The KEK Secret is created by the API on its first start (KOINON_KEK_CREATE).
			Expect(cl.Secret(ctx, kekSecretName(r))).NotTo(BeNil(), "the key-encryption Secret exists")
		}, SpecTimeout(3*time.Minute))

		// The install binding (ADR 0110): koinon-api reads the install mark from the same Secret as the
		// reconciler and agentd, and its first unheld start claims the one install_binding row with the
		// hash of that mark. A koinon-api without KOINON_INSTALL_MARK would bind nothing and refuse nothing.
		It("gives koinon-api the install mark and binds the database to it", func(ctx SpecContext) {
			c := r.deployment("koinon-api").Spec.Template.Spec.Containers[0]
			var ref *corev1.EnvVar
			for i := range c.Env {
				if c.Env[i].Name == "KOINON_INSTALL_MARK" {
					ref = &c.Env[i]
				}
			}
			Expect(ref).NotTo(BeNil(), "koinon-api has KOINON_INSTALL_MARK")
			Expect(ref.ValueFrom).NotTo(BeNil())
			Expect(ref.ValueFrom.SecretKeyRef).NotTo(BeNil())
			Expect(ref.ValueFrom.SecretKeyRef.Key).To(Equal("token"))

			conn, err := pgx.Connect(ctx, dbURL)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(conn.Close)
			// A held start (--restore-hold, ADR 0107) writes an unbound row: state copy, held_since set
			// (ADR 0110 decision 0). An unheld first start claims the row with its own mark.
			held := slices.Contains(c.Args, "--restore-hold")
			for _, e := range c.Env {
				if e.Name == "KOINON_RESTORE_HOLD" && e.Value == "true" {
					held = true
				}
			}
			var bound, heldSince bool
			Expect(conn.QueryRow(ctx, `SELECT mark IS NOT NULL, held_since IS NOT NULL FROM install_binding WHERE singleton`).Scan(&bound, &heldSince)).To(Succeed(),
				"koinon-api wrote the install_binding row on its first start")
			if held {
				Expect(bound).To(BeFalse(), "a held first start writes an unbound row (state copy)")
				Expect(heldSince).To(BeTrue(), "a held first start sets held_since")
			} else {
				Expect(bound).To(BeTrue(), "an unheld first start binds the row to its own mark")
			}
		}, SpecTimeout(time.Minute))

		// The install audit chain (ADR 0100): the group role existed before the migration, the login
		// roles came after it, and koinon-api, which connects as koinon_api, can append to the chain.
		// A first start in mode auto records nothing (the default is not a change), so the event is
		// asserted where the values file turns the refresh on; the privilege checks run in every mode.
		It("lets koinon-api, and only it, append to the install audit chain", func(ctx SpecContext) {
			conn, err := pgx.Connect(ctx, dbURL)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(conn.Close)

			var n int
			args := r.deployment("koinon-api").Spec.Template.Spec.Containers[0].Args
			if i := slices.Index(args, "--prices-refresh"); i >= 0 && i+1 < len(args) && args[i+1] != "auto" {
				Expect(conn.QueryRow(ctx, `SELECT count(*) FROM install_audit_events
					WHERE action = 'price.refresh.settings_changed' AND actor_kind = 'system' AND actor_id = 'koinon-api:price-refresh'`).Scan(&n)).To(Succeed())
				Expect(n).To(Equal(1), "koinon-api wrote an install event on its first start, as koinon_api")
			}

			Expect(insertInstallEventAs(ctx, dbURL, "koinon_api")).To(Succeed(), "koinon_api is in the writer group")
			Expect(installWriterOK(ctx, ready["koinon-api"][0])).To(Equal("1"), "koinon-api found its own login in the writer group")
			for _, role := range []string{"koinon_reconciler", "koinon_agentd"} {
				err := insertInstallEventAs(ctx, dbURL, role)
				Expect(err).To(HaveOccurred(), "%s may not append to the install audit chain", role)
				Expect(err.Error()).To(ContainSubstring("permission denied"), role)
			}
		}, SpecTimeout(time.Minute))

		// A koinon-api that starts before the writer grant (#328 review): it gets ready, the changed
		// price refresh setting is not recorded (its install event is refused) and the scheduled refresh
		// runs in the recorded mode; once the grant lands the retry records the setting with exactly
		// one event, and the refresh switches to the configured mode. Only a values file that turns the
		// refresh on can show it: the stored row is set back to auto to make the start a change.
		It("starts koinon-api before the writer grant and records the refresh setting once it lands", func(ctx SpecContext) {
			args := r.deployment("koinon-api").Spec.Template.Spec.Containers[0].Args
			i := slices.Index(args, "--prices-refresh")
			if i < 0 || i+1 >= len(args) || args[i+1] != "on" {
				Skip("the values file leaves the price refresh in auto mode")
			}
			conn, err := pgx.Connect(ctx, dbURL)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(conn.Close)
			count := func(ctx context.Context) int {
				var n int
				Expect(conn.QueryRow(ctx, `SELECT count(*) FROM install_audit_events
					WHERE action = 'price.refresh.settings_changed' AND actor_id = 'koinon-api:price-refresh'`).Scan(&n)).To(Succeed())
				return n
			}
			storedMode := func(ctx context.Context) string {
				var m string
				Expect(conn.QueryRow(ctx, `SELECT mode FROM price_refresh_settings`).Scan(&m)).To(Succeed())
				return m
			}
			before := count(ctx)

			By("the grant is missing and the recorded setting is auto; koinon-api restarts")
			for _, stmt := range []string{`REVOKE koinon_install_audit_writer FROM koinon_api`, `UPDATE price_refresh_settings SET mode = 'auto'`} {
				_, err := conn.Exec(ctx, stmt)
				Expect(err).NotTo(HaveOccurred(), stmt)
			}
			for _, w := range ready["koinon-api"] {
				Expect(w.Ctr.Stop(ctx, &stopTimeout)).To(Succeed())
			}
			ready["koinon-api"] = startDeployment(ctx, rn, r, "koinon-api")
			api := ready["koinon-api"][0]

			By("it is ready, its setting is not recorded, and the refresh runs in the recorded mode")
			Eventually(func(ctx context.Context) string { return api.Logs(ctx) }).WithContext(ctx).WithTimeout(30 * time.Second).Should(And(
				ContainSubstring("the price refresh setting is not recorded yet"),
				ContainSubstring("runs in the recorded mode until koinon-api's setting is recorded")))

			By("koinon-api checked its own login: ready, the gauge reads 0, and the log names the GRANT for it")
			Expect(installWriterOK(ctx, api)).To(Equal("0"))
			Expect(api.Logs(ctx)).To(ContainSubstring(`GRANT koinon_install_audit_writer TO "koinon_api"`))
			Expect(api.Logs(ctx)).NotTo(ContainSubstring("runs in the configured mode"), "no tick in mode on before the event")
			Expect(storedMode(ctx)).To(Equal("auto"))
			Expect(count(ctx)).To(Equal(before))

			By("the grant lands: the retry records the setting with one event")
			_, err = conn.Exec(ctx, `GRANT koinon_install_audit_writer TO koinon_api`)
			Expect(err).NotTo(HaveOccurred())
			Eventually(count).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Equal(before + 1))
			Expect(storedMode(ctx)).To(Equal("on"))
			Consistently(count).WithContext(ctx).WithTimeout(5 * time.Second).WithPolling(time.Second).Should(Equal(before + 1))
			Expect(api.Logs(ctx)).To(ContainSubstring("recorded after a retry"))
			Eventually(func(ctx context.Context) string { return installWriterOK(ctx, api) }).WithContext(ctx).WithTimeout(time.Minute).WithPolling(2*time.Second).
				Should(Equal("1"), "the next 30-second check sees the grant")

			By("the next scheduled tick runs in the configured mode")
			Eventually(func(ctx context.Context) string { return api.Logs(ctx) }).WithContext(ctx).WithTimeout(90 * time.Second).WithPolling(2 * time.Second).
				Should(ContainSubstring("runs in the configured mode"))
			expectRunning(ctx, api, "koinon-api still serves; logs:\n%s", api.Logs(ctx))
		}, SpecTimeout(4*time.Minute))

		// An upgrade, in cnpg mode, of a cluster initialised before ADR 0100 (#328): the pre-upgrade
		// migrate Job runs before the updated Cluster creates the managed group role, so it finds no role
		// and grants nothing; CNPG then creates the role (and adds koinon_api to it) on its own schedule,
		// and the post-upgrade hook, which waits for the role, grants it. The hook is rendered in cnpg
		// mode with --is-upgrade; it connects as the migrate Job does here (the cnpg URI verifies the
		// CNPG cluster's CA, see externalDatabase), the rest of its pod spec is the chart's.
		It("grants the writer role once an upgraded cluster creates it, in the post-upgrade hook", func(ctx SpecContext) {
			conn, err := pgx.Connect(ctx, dbURL)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(conn.Close)
			execAll := func(stmts ...string) {
				GinkgoHelper()
				for _, stmt := range stmts {
					_, err := conn.Exec(ctx, stmt)
					Expect(err).NotTo(HaveOccurred(), stmt)
				}
			}

			By("a cluster from before ADR 0100: no writer role, koinon_api in koinon_app only")
			execAll(
				`REVOKE ALL ON install_audit_events, install_audit_chain_head, install_audit_anchor_heads FROM koinon_install_audit_writer`,
				`REVOKE koinon_install_audit_writer FROM koinon_api`,
				`DROP ROLE koinon_install_audit_writer`,
			)
			Expect(insertInstallEventAs(ctx, dbURL, "koinon_api")).To(MatchError(ContainSubstring("permission denied")))

			var migrate *batchv1.Job
			for _, j := range r.Jobs {
				if j.Name == "koinon-migrate" {
					migrate = j
				}
			}
			Expect(migrate).NotTo(BeNil())

			// The check runs before any migration (no migration was applied). This database is migrated
			// already, so goose's version table is the witness that the failed Job left it as it was;
			// the Docker spec of internal/cmd/koinonapi proves it on an empty database.
			migrations := func() int {
				GinkgoHelper()
				var n int
				Expect(conn.QueryRow(ctx, `SELECT count(*) FROM goose_db_version`).Scan(&n)).To(Succeed())
				return n
			}
			before := migrations()

			By("on an external database (this render) the migrate Job fails without the role, naming the remedy")
			mc := migrate.Spec.Template.Spec.Containers[0]
			Expect(mc.Args).To(ContainElement("--require-install-writer"))
			w := rn.start(ctx, migrate.Name, migrate.Spec.Template.Spec, mc, jobDeadline)
			Expect(w.ExitCode(ctx)).NotTo(BeZero(), "logs:\n%s", w.Logs(ctx))
			Expect(w.Logs(ctx)).To(And(ContainSubstring("the role koinon_install_audit_writer does not exist"), ContainSubstring(project.SecretKeysDocURL),
				ContainSubstring("no migration was applied")))
			Expect(migrations()).To(Equal(before))

			By("the cnpg pre-upgrade migrate Job finds no role, grants nothing and does not fail the upgrade")
			// In cnpg mode the chart does not pass --require-install-writer (hack/chart-check.d/45 and
			// TestChartContract pin both renders); the rest of its pod spec is this one.
			migrate = migrate.DeepCopy()
			c := &migrate.Spec.Template.Spec.Containers[0]
			c.Args = slices.DeleteFunc(c.Args, func(a string) bool { return a == "--require-install-writer" })
			runJob(ctx, rn, migrate)
			Expect(insertInstallEventAs(ctx, dbURL, "koinon_api")).To(MatchError(ContainSubstring("permission denied")))

			By("the post-upgrade hook waits while CNPG reconciles its managed roles, then grants")
			up := helmRenderWith(ctx, valuesFile, cnpgUpgrade...)
			Expect(up.Jobs).To(HaveLen(1))
			grants := up.Jobs[0].DeepCopy()
			Expect(grants.Annotations).To(HaveKeyWithValue("helm.sh/hook", "post-upgrade"))
			Expect(grants.Annotations).To(HaveKeyWithValue("argocd.argoproj.io/hook", "PostSync"))
			Expect(grants.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--grants-only"))
			ps, mps := &grants.Spec.Template.Spec, migrate.Spec.Template.Spec
			ps.Volumes = append(slices.DeleteFunc(slices.Clone(ps.Volumes), func(v corev1.Volume) bool { return v.Name == "db-ca" || v.Name == "tmp" }), mps.Volumes...)
			ps.Containers[0].Env = mps.Containers[0].Env
			ps.Containers[0].VolumeMounts = mps.Containers[0].VolumeMounts
			for _, o := range up.Objects {
				Expect(applyObject(ctx, cl, o)).To(Succeed(), "the hook's ServiceAccount, a hook of a lower weight")
			}
			created := make(chan error, 1)
			go func() {
				defer GinkgoRecover()
				select {
				case <-ctx.Done():
					created <- ctx.Err()
					return
				case <-time.After(3 * time.Second):
				}
				_, err := conn.Exec(ctx, `CREATE ROLE koinon_install_audit_writer NOLOGIN`)
				if err == nil {
					_, err = conn.Exec(ctx, `GRANT koinon_install_audit_writer TO koinon_api`)
				}
				created <- err
			}()
			runJob(ctx, rn, grants)
			Expect(<-created).To(Succeed(), "CNPG's role reconcile, simulated")

			By("koinon_api can now append to the install audit chain, without another upgrade")
			Expect(insertInstallEventAs(ctx, dbURL, "koinon_api")).To(Succeed())
			Expect(insertInstallEventAs(ctx, dbURL, "koinon_reconciler")).To(MatchError(ContainSubstring("permission denied")))
		}, SpecTimeout(3*time.Minute))

		// The MCP leak guard (ADR 0091): agentgateway fails every org MCP call closed when it cannot
		// reach the guard, so the guard koinon-api serves must be where the chart's reconciler config
		// points, and answer there.
		It("serves the MCP leak guard where the reconciler's config points", func(ctx SpecContext) {
			Expect(ready["koinon-api"]).NotTo(BeEmpty(), "koinon-api is running")
			checkLeakGuard(ctx, r, ready["koinon-api"][0])
		}, SpecTimeout(time.Minute))

		It("starts koinon-reconciler and answers its probes", func(ctx SpecContext) {
			ready["koinon-reconciler"] = startDeployment(ctx, rn, r, "koinon-reconciler")
			accounted = append(accounted, "koinon-reconciler")
			// The reconciler read mcpGuard.url from the chart's config: without it it warns and renders
			// a gateway that refuses every org MCP call.
			for _, w := range ready["koinon-reconciler"] {
				Expect(w.Logs(ctx)).NotTo(ContainSubstring("no --mcp-guard-url"), "the reconciler gets the leak guard's URL")
			}
		}, SpecTimeout(3*time.Minute))

		// The provisioner (ADR 0128, issue 1398) is "koinon-reconciler provision". It must start with the
		// rendered args, answer /healthz, and be allowed to create a TokenReview: a request with a token
		// that is not valid makes it review that token, which must end in 401 and never in a Forbidden answer.
		It("starts koinon-provisioner, answers /healthz and may create a TokenReview", func(ctx SpecContext) {
			d := r.deployment("koinon-provisioner")
			if d == nil {
				Skip("execution.substrate.provisioner is not rendered in this values file")
			}
			ws := startDeployment(ctx, rn, r, "koinon-provisioner")
			ready["koinon-provisioner"] = ws
			accounted = append(accounted, "koinon-provisioner")
			Expect(ws).To(HaveLen(1))
			w := ws[0]
			port := w.portNumber(intOrName{name: "http"})
			mapped, err := w.Ctr.MappedPort(ctx, fmt.Sprintf("%d/tcp", port))
			Expect(err).NotTo(HaveOccurred())
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+mapped.Port()+"/v1/atespaces", strings.NewReader(`{"name":"kw-smoke"}`))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer koinon_test_not-a-real-token")
			resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
			Expect(err).NotTo(HaveOccurred(), "logs:\n%s", w.Logs(ctx))
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized), "a token that is not valid is refused; logs:\n%s", w.Logs(ctx))
			logs := w.Logs(ctx)
			Expect(logs).To(ContainSubstring("token not accepted"), "the provisioner reviewed the token; logs:\n%s", logs)
			Expect(logs).To(ContainSubstring("token review: not authenticated"), "the API server answered the review (no transport error); logs:\n%s", logs)
			Expect(strings.ToLower(logs)).NotTo(ContainSubstring("forbidden"), "the provisioner may create a TokenReview; logs:\n%s", logs)
			expectRunning(ctx, w, "still running; logs:\n%s", logs)
			// The reconciler started above with the provisioner-token mount and the provisioner url and tokenFile keys.
			Expect(ready["koinon-reconciler"]).NotTo(BeEmpty())
		}, SpecTimeout(3*time.Minute))

		It("starts koinon-agentd and answers its probes", func(ctx SpecContext) {
			ready["koinon-agentd"] = startDeployment(ctx, rn, r, "koinon-agentd")
			accounted = append(accounted, "koinon-agentd")
			// Its readiness probe is /readyz on the health port: the schema gate (ADR 0085, old issue 408)
			// reads goose_db_version and schema_compat of the migrated database and must find the schema supported.
			// The drain on SIGTERM must fit in the pod's grace period (ADR 0085): the kubelet
			// would otherwise kill agentd mid-drain and the leases would not be released.
			ps := r.deployment("koinon-agentd").Spec.Template.Spec
			var drain time.Duration
			var agentMCP string
			for _, c := range ps.Containers {
				for _, e := range c.Env {
					switch e.Name {
					case "KOINON_DRAIN_DEADLINE":
						d, err := time.ParseDuration(e.Value)
						Expect(err).NotTo(HaveOccurred())
						drain = d
					case "KOINON_AGENT_MCP_URL":
						agentMCP = e.Value
					}
				}
			}
			Expect(drain).To(BeNumerically(">", 0), "koinon-agentd gets KOINON_DRAIN_DEADLINE")
			// The agents' MCP URL (#1239): koinon-gateway's /mcp with gateway.mcp.driver=koinon, and unset with
			// agentgateway (agentd then uses <KOINON_GATEWAY_URL>/mcp). agentd started with it above.
			if r.deployment("koinon-mcp-gateway") != nil {
				Expect(agentMCP).To(HavePrefix("http://koinon-mcp-gateway."), "koinon-agentd gets koinon-gateway's MCP URL")
				Expect(agentMCP).To(HaveSuffix("/mcp"))
			} else {
				Expect(agentMCP).To(BeEmpty(), "koinon-agentd gets no MCP URL with the agentgateway MCP driver")
			}
			Expect(ps.TerminationGracePeriodSeconds).NotTo(BeNil())
			Expect(time.Duration(*ps.TerminationGracePeriodSeconds) * time.Second).To(BeNumerically(">=", drain+30*time.Second))
		}, SpecTimeout(3*time.Minute))

		// koinon-gateway (the MCP path, gateway.mcp.driver=koinon). The Docker network has no Service DNS,
		// so it cannot reach koinon-api's State stream and must stay unready unless held (issue #1358): that keeps a rollout
		// from taking old pods away before a new pod can serve. It must still run, answer its liveness
		// probe and serve /metrics, with the args, the mounts and the environment the chart gives it.
		It("starts koinon-mcp-gateway, alive; ready only when held, without a config", func(ctx SpecContext) {
			d := r.deployment("koinon-mcp-gateway")
			if d == nil {
				Skip("gateway.mcp.driver is not koinon in this values file")
			}
			Expect(d.Spec.Replicas).NotTo(BeNil())
			Expect(*d.Spec.Replicas).To(BeNumerically(">=", 2), "the MCP gateway runs two replicas at least")
			ps := d.Spec.Template.Spec
			Expect(ps.Containers).To(HaveLen(1))
			c := ps.Containers[0]
			w := rn.start(ctx, "koinon-mcp-gateway", ps, c, 0)
			ready["koinon-mcp-gateway"] = []*workload{w}
			accounted = append(accounted, "koinon-mcp-gateway")
			Expect(c.LivenessProbe).NotTo(BeNil())
			Expect(w.ProbeHTTP(ctx, c.LivenessProbe, probeDeadline)).To(BeEmpty(), "liveness; logs:\n%s", w.Logs(ctx))
			metrics := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/metrics", Port: intstr.FromString("stats")}}}
			Expect(w.ProbeHTTP(ctx, metrics, probeDeadline)).To(BeEmpty(), "/metrics; logs:\n%s", w.Logs(ctx))
			// The leak guard's own login (koinon_mcpgateway, in koinon_gateway only) reaches the database
			// through --database-url-file, and the migration's grants let it read what the guard reads.
			Eventually(func() string { return w.Logs(ctx) }).WithTimeout(time.Minute).
				Should(ContainSubstring("the leak guard reached its database"), "the gateway's login and grants work; logs:\n%s", w.Logs(ctx))
			if slices.ContainsFunc(c.Args, func(a string) bool { return strings.HasPrefix(a, "--egress-addr=") }) {
				// The egress listener (#1086 part C2) loads the public actor token key that the ensure-secrets
				// Job made, before any listener starts; a missing or bad key stops the pod. With the Substrate
				// verifier (#1386) it loads the issuer's CA instead and reads the keys on the first token.
				want := `egress routes loaded"?,? ?"?actor_token_verifier"?[=:]"?ed25519"?,? ?"?keys"?[=:]1\b`
				if slices.Contains(c.Args, "--actor-token-verifier=substrate") {
					want = `egress routes loaded"?,? ?"?actor_token_verifier"?[=:]"?substrate"?,? ?"?keys"?[=:]0\b`
				}
				Eventually(func() string { return w.Logs(ctx) }).WithTimeout(time.Minute).
					Should(MatchRegexp(want), "the egress listener loaded its actor token check; logs:\n%s", w.Logs(ctx))
			}
			Expect(c.ReadinessProbe).NotTo(BeNil())
			status, why := w.ProbeStatus(ctx, c.ReadinessProbe, 10*time.Second)
			Expect(why).To(BeEmpty(), "readiness endpoint did not answer; logs:\n%s", w.Logs(ctx))
			if slices.Contains(c.Args, "--restore-hold") {
				// A held gateway is ready but deny-all: the held reconciler publishes no revision (issue #1358).
				Expect(status).To(Equal(http.StatusOK), "held and ready with no config (200 from /readyz); logs:\n%s", w.Logs(ctx))
				if slices.ContainsFunc(c.Args, func(a string) bool { return strings.HasPrefix(a, "--egress-addr=") }) {
					// The held egress listener serves no request either (issue #1390): 503, reason held, before
					// the actor token check and the route match.
					eg := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
						Path: "/egress/registry.example.test/held", Port: intstr.FromString("egress"),
					}}}
					egStatus, why := w.ProbeStatus(ctx, eg, 10*time.Second)
					Expect(why).To(BeEmpty(), "the egress listener did not answer; logs:\n%s", w.Logs(ctx))
					Expect(egStatus).To(Equal(http.StatusServiceUnavailable), "a held egress listener refuses every request; logs:\n%s", w.Logs(ctx))
					Eventually(func() string { return w.Logs(ctx) }).WithTimeout(time.Minute).
						Should(MatchRegexp(`egress request.*"?result"?[=:]"?held\b`), "the refusal is logged as held; logs:\n%s", w.Logs(ctx))
				}
			} else {
				Expect(status).To(Equal(http.StatusServiceUnavailable), "unready before any config applied (503 from /readyz); logs:\n%s", w.Logs(ctx))
			}
			expectRunning(ctx, w, "still running; logs:\n%s", w.Logs(ctx))
			// The drain on SIGTERM must fit in the grace period, with 30 seconds to exit.
			Expect(ps.TerminationGracePeriodSeconds).NotTo(BeNil())
			Expect(*ps.TerminationGracePeriodSeconds).To(BeNumerically(">=", 60))
		}, SpecTimeout(3*time.Minute))

		// koinon-gateway (the model path, gateway.model.driver=koinon). The Docker network has no Service DNS,
		// so it cannot reach koinon-api's State stream and must stay unready unless held (issue #1358): that keeps a rollout
		// from taking old pods away before a new pod can serve. It must still run, answer its liveness
		// probe and serve /metrics, with the args, the mounts and the environment the chart gives it.
		It("starts koinon-model-gateway, alive; ready only when held, without a config", func(ctx SpecContext) {
			d := r.deployment("koinon-model-gateway")
			if d == nil {
				Skip("gateway.model.driver is not koinon in this values file")
			}
			Expect(d.Spec.Replicas).NotTo(BeNil())
			Expect(*d.Spec.Replicas).To(BeNumerically(">=", 2), "the model gateway runs two replicas at least")
			ps := d.Spec.Template.Spec
			Expect(ps.Containers).To(HaveLen(1))
			c := ps.Containers[0]
			w := rn.start(ctx, "koinon-model-gateway", ps, c, 0)
			ready["koinon-model-gateway"] = []*workload{w}
			accounted = append(accounted, "koinon-model-gateway")
			Expect(c.LivenessProbe).NotTo(BeNil())
			Expect(w.ProbeHTTP(ctx, c.LivenessProbe, probeDeadline)).To(BeEmpty(), "liveness; logs:\n%s", w.Logs(ctx))
			metrics := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/metrics", Port: intstr.FromString("stats")}}}
			Expect(w.ProbeHTTP(ctx, metrics, probeDeadline)).To(BeEmpty(), "/metrics; logs:\n%s", w.Logs(ctx))
			Expect(c.ReadinessProbe).NotTo(BeNil())
			status, why := w.ProbeStatus(ctx, c.ReadinessProbe, 10*time.Second)
			Expect(why).To(BeEmpty(), "readiness endpoint did not answer; logs:\n%s", w.Logs(ctx))
			if slices.Contains(c.Args, "--restore-hold") {
				// A held gateway is ready but deny-all: the held reconciler publishes no revision (issue #1358).
				Expect(status).To(Equal(http.StatusOK), "held and ready with no config (200 from /readyz); logs:\n%s", w.Logs(ctx))
			} else {
				Expect(status).To(Equal(http.StatusServiceUnavailable), "unready before any config applied (503 from /readyz); logs:\n%s", w.Logs(ctx))
			}
			expectRunning(ctx, w, "still running; logs:\n%s", w.Logs(ctx))
			// The drain on SIGTERM must fit in the grace period, with 30 seconds to exit.
			Expect(ps.TerminationGracePeriodSeconds).NotTo(BeNil())
			Expect(*ps.TerminationGracePeriodSeconds).To(BeNumerically(">=", 60))
		}, SpecTimeout(3*time.Minute))

		It("keeps every Deployment running and permitted", func(ctx SpecContext) {
			time.Sleep(settleTime)
			for name, ws := range ready {
				for _, w := range ws {
					logs := w.Logs(ctx)
					expectRunning(ctx, w, "%s (%s) stopped within %s of being ready (exit code %d); logs:\n%s", w.Name, name, settleTime, w.ExitCode(ctx), logs)
					// No restarts happen here, so a running container has restart count 0.
					Expect(strings.ToLower(logs)).NotTo(ContainSubstring("forbidden"),
						"%s was denied by the API server: the chart's RBAC lacks a rule; logs:\n%s", w.Name, logs)
				}
			}
		}, SpecTimeout(2*time.Minute))

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

// hookOf reads a Helm hook annotation: whether the object is a hook, whether it is a pre-install
// hook, and its weight.
func hookOf(m metav1.Object) (weight int, pre, hook bool) {
	a := m.GetAnnotations()
	h, ok := a["helm.sh/hook"]
	if !ok {
		return 0, false, false
	}
	pre = slices.Contains(strings.Split(h, ","), "pre-install")
	if w := a["helm.sh/hook-weight"]; w != "" {
		n, err := strconv.Atoi(w)
		Expect(err).NotTo(HaveOccurred(), "hook weight of %s", m.GetName())
		weight = n
	}
	return weight, pre, true
}

func mustMeta(o runtime.Object) metav1.Object {
	m, err := apimeta.Accessor(o)
	if err != nil {
		panic(err)
	}
	return m
}

func kindOf(o runtime.Object) string { return fmt.Sprintf("%T", o) }

// kekSecretName is the key-encryption Secret the API uses (KOINON_KEK_SECRET).
func kekSecretName(r *rendered) string {
	for _, c := range r.deployment("koinon-api").Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "KOINON_KEK_SECRET" {
				return e.Value
			}
		}
	}
	panic("koinon-api has no KOINON_KEK_SECRET")
}

// runJob runs every container of the Job's pod template to completion and requires exit code 0
// (for a platform Job: its report, and no Forbidden answer from the API server, which would mean
// the chart's RBAC lacks a rule).
func runJob(ctx context.Context, rn *runner, job *batchv1.Job) {
	GinkgoHelper()
	ps := job.Spec.Template.Spec
	for _, c := range append(slices.Clone(ps.InitContainers), ps.Containers...) {
		// The gateway key-admin Job waits (--wait-loaded) until a gateway process reports that it holds
		// the new key. No gateway runs while the plain Jobs run, so the wait cannot end here: the Job
		// runs without it and stops at the pending key. The wait has its own specs in internal/gatewaykeys.
		if strings.Contains(job.Name, "-gateway-key-admin-") {
			c.Args = slices.DeleteFunc(slices.Clone(c.Args), func(a string) bool { return strings.HasPrefix(a, "--wait-loaded") })
		}
		w := rn.start(ctx, job.Name, ps, c, jobDeadline)
		code, logs := w.ExitCode(ctx), w.Logs(ctx)
		AddReportEntry("log "+w.Name, logs, ReportEntryVisibilityFailureOrVerbose)
		if p, ok := platformJobs[job.Name]; ok {
			Expect(logs).To(ContainSubstring(p.report), "%s did not reach its checks (exit %d, args %q)", w.Name, code, c.Args)
			Expect(strings.ToLower(logs)).NotTo(ContainSubstring("forbidden"), "%s was denied by the API server: the chart's RBAC lacks a rule", w.Name)
			Expect(code).To(BeElementOf(0, 1), "%s exited with code %d", w.Name, code)
			continue
		}
		Expect(code).To(Equal(0), "%s exited with code %d (args %q); logs:\n%s", w.Name, code, c.Args, logs)
	}
}

// installWriterOK is the value of koinon_install_audit_writer_ok on the koinon-api container's
// metrics port ("" when it is not exported).
func installWriterOK(ctx context.Context, api *workload) string {
	GinkgoHelper()
	mapped, err := api.Ctr.MappedPort(ctx, fmt.Sprintf("%d/tcp", api.portNumber(intOrName{name: "metrics"})))
	Expect(err).NotTo(HaveOccurred())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+mapped.Port()+"/metrics", nil)
	Expect(err).NotTo(HaveOccurred())
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	for _, line := range strings.Split(string(body), "\n") {
		if v, ok := strings.CutPrefix(line, "koinon_install_audit_writer_ok "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// startDeployment starts each container of the Deployment's pod template and requires its
// probes (startup, readiness, liveness) to answer within a minute. A container without HTTP
// probes must serve /metrics on its "metrics" port instead, which the chart scrapes.
func startDeployment(ctx context.Context, rn *runner, r *rendered, name string) []*workload {
	GinkgoHelper()
	d := r.deployment(name)
	Expect(d).NotTo(BeNil(), "%s is rendered", name)
	ps := d.Spec.Template.Spec
	var started []*workload
	for _, c := range ps.Containers {
		w := rn.start(ctx, name, ps, c, 0)
		started = append(started, w)
		any := false
		for _, p := range []*corev1.Probe{c.StartupProbe, c.ReadinessProbe, c.LivenessProbe} {
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
		expectRunning(ctx, w, "%s is still running; logs:\n%s", w.Name, w.Logs(ctx))
	}
	return started
}

// applyObject creates a ServiceAccount, Secret, ConfigMap, Deployment or RBAC object in the API server.
func applyObject(ctx context.Context, cl *cluster, o runtime.Object) error {
	c := cl.Admin
	var err error
	switch o := o.(type) {
	case *corev1.ServiceAccount:
		_, err = c.CoreV1().ServiceAccounts(namespace).Create(ctx, o, metav1.CreateOptions{})
	case *corev1.ConfigMap:
		_, err = c.CoreV1().ConfigMaps(namespace).Create(ctx, o, metav1.CreateOptions{})
	case *corev1.Secret:
		_, err = c.CoreV1().Secrets(namespace).Create(ctx, o, metav1.CreateOptions{})
	case *appsv1.Deployment:
		_, err = c.AppsV1().Deployments(namespace).Create(ctx, o, metav1.CreateOptions{})
	case *rbacv1.Role:
		_, err = c.RbacV1().Roles(objectNamespace(o)).Create(ctx, o, metav1.CreateOptions{})
	case *rbacv1.RoleBinding:
		_, err = c.RbacV1().RoleBindings(objectNamespace(o)).Create(ctx, o, metav1.CreateOptions{})
	case *rbacv1.ClusterRole:
		_, err = c.RbacV1().ClusterRoles().Create(ctx, o, metav1.CreateOptions{})
	case *rbacv1.ClusterRoleBinding:
		_, err = c.RbacV1().ClusterRoleBindings().Create(ctx, o, metav1.CreateOptions{})
	default:
		return fmt.Errorf("applyObject: %T is not modelled", o)
	}
	return err
}

// insertInstallEventAs appends a probe event to the install audit chain as the login role.
func insertInstallEventAs(ctx context.Context, dbURL, role string) error {
	GinkgoHelper()
	c, err := pgx.Connect(ctx, strings.Replace(dbURL, "//koinon:", "//"+role+":", 1))
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = c.Close(ctx) }()
	// audit-lint:allow tests the chart's grants: the insert as the login role must succeed or be refused by the database
	_, err = c.Exec(ctx, `INSERT INTO install_audit_events (actor_kind, actor_id, action, target_kind, target_id, summary)
		VALUES ('system', 'koinon-test:chartsmoke', 'chartsmoke.probe', 'probe', 'x', '{}')`)
	return err
}

// bootstrapDatabase is what the chart's CNPG bootstrap (postInitApplicationSQL in
// templates/postgres-cnpg.yaml) does before any migration runs: the group roles, in the chart's
// order, and the owner's default privileges. hack/chart-check.d/45-install-audit-writer-role.sh
// pins the chart side of the roles.
func bootstrapDatabase(ctx context.Context, dbURL string) {
	GinkgoHelper()
	conn, err := pgx.Connect(ctx, dbURL)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = conn.Close(ctx) }()
	for _, stmt := range []string{
		`CREATE ROLE koinon_app NOLOGIN`,
		`CREATE ROLE koinon_install_audit_writer NOLOGIN`,
		`CREATE ROLE koinon_gateway NOLOGIN`,
		// The gateway key lifecycle (ADR 0097 decision 12): the key-admin Job's and the re-seal group roles.
		`CREATE ROLE koinon_gateway_key_admin NOLOGIN`,
		`CREATE ROLE koinon_gateway_reseal NOLOGIN`,
		`GRANT CONNECT ON DATABASE koinon TO koinon_app`,
		`GRANT USAGE ON SCHEMA public TO koinon_app`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE koinon IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO koinon_app`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE koinon IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO koinon_app`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE koinon IN SCHEMA public GRANT EXECUTE ON FUNCTIONS TO koinon_app`,
	} {
		_, err := conn.Exec(ctx, stmt)
		Expect(err).NotTo(HaveOccurred(), stmt)
	}
}

// createComponentRoles creates the managed login roles of the chart (inRoles as rendered): all in
// koinon_app, koinon_api also in the install audit writer group.
func createComponentRoles(ctx context.Context, dbURL string) {
	GinkgoHelper()
	conn, err := pgx.Connect(ctx, dbURL)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = conn.Close(ctx) }()
	for _, c := range []struct{ role, groups string }{
		{"koinon_api", "koinon_app, koinon_install_audit_writer"},
		{"koinon_reconciler", "koinon_app"},
		{"koinon_agentd", "koinon_app"},
		// koinon-gateway's own login: in the gateway group only, never in koinon_app.
		{"koinon_mcpgateway", "koinon_gateway"},
		// The gateway key lifecycle logins: each in its own group role only.
		{"koinon_gwkeyadmin", "koinon_gateway_key_admin"},
		{"koinon_reseal", "koinon_gateway_reseal"},
	} {
		stmt := fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' IN ROLE %s`, c.role, dbSecretPassword, c.groups)
		_, err := conn.Exec(ctx, stmt)
		Expect(err).NotTo(HaveOccurred(), c.role)
	}
}

// objectNamespace is the namespace of a rendered Role or RoleBinding: the release namespace unless the chart names another.
func objectNamespace(o metav1.Object) string {
	if ns := o.GetNamespace(); ns != "" {
		return ns
	}
	return namespace
}

// seedSubstrateNamespace makes the Substrate namespace and the atelet DaemonSet as "koinon-api substrate install" leaves
// them (issue #1191): the DaemonSet has the optional ConfigMap volume and SSL_CERT_DIR of the installer patch. No pod runs.
func seedSubstrateNamespace(ctx context.Context, cl *cluster, ns string) {
	GinkgoHelper()
	_, err := cl.Admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	optional := true
	labels := map[string]string{"app": imagecacheca.DaemonSetName}
	_, err = cl.Admin.AppsV1().DaemonSets(ns).Create(ctx, &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: imagecacheca.DaemonSetName},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:         imagecacheca.AteletContainer,
						Image:        "registry.invalid/koinon-smoke/atelet:stub",
						Env:          []corev1.EnvVar{{Name: imagecacheca.EnvName, Value: imagecacheca.CertDirs}},
						VolumeMounts: []corev1.VolumeMount{{Name: imagecacheca.VolumeName, MountPath: imagecacheca.MountPath, ReadOnly: true}},
					}},
					Volumes: []corev1.Volume{{Name: imagecacheca.VolumeName, VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: imagecacheca.ConfigMapName}, Optional: &optional},
					}}},
				},
			},
		},
	}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
}
