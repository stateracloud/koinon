// SPDX-License-Identifier: Apache-2.0

package drill_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
)

// A failed spec leaves what the run needs to be read afterwards: the installs are removed at the end of
// the suite, so by the time run.sh looks the pods and their logs are gone. The dump is best effort and
// never fails a spec; it goes to $KOINON_DRILL_OUT/diag-<step>.txt.
var _ = JustAfterEach(func() {
	rep := CurrentSpecReport()
	if !rep.Failed() || kube == nil || top == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var b strings.Builder
	section := func(title, body string, err error) {
		fmt.Fprintf(&b, "\n===== %s\n%s\n", title, strings.TrimSpace(body))
		if err != nil {
			fmt.Fprintf(&b, "(error: %v)\n", err)
		}
	}
	for _, ns := range []string{top.Source.Namespace, top.Target.Namespace, top.FakesNamespace, top.Source.GatewayNamespace, top.Target.GatewayNamespace} {
		if out, err := kube.Kubectl(ctx, "get", "namespace", ns, "-o", "name"); err != nil || out == "" {
			continue // not created yet
		}
		out, err := kube.Kubectl(ctx, "-n", ns, "get", "pods", "-o", "wide")
		section("pods "+ns, out, err)
		out, err = kube.Kubectl(ctx, "-n", ns, "get", "events", "--sort-by=.lastTimestamp")
		section("events "+ns, out, err)
		names, _ := kube.Kubectl(ctx, "-n", ns, "get", "pods", "-o", "name")
		for _, p := range strings.Fields(names) {
			out, err := kube.Kubectl(ctx, "-n", ns, "logs", p, "--all-containers", "--prefix", "--tail=120")
			section("logs "+ns+" "+p, out, err)
		}
	}
	out, err := kube.Kubectl(ctx, "get", "sandboxes.agents.x-k8s.io", "-A", "-o", "wide")
	section("sandboxes", out, err)
	if src.in.Namespace != "" {
		for _, q := range []string{
			"SELECT to_jsonb(r) FROM (SELECT * FROM runs ORDER BY id DESC LIMIT 5) r",
			"SELECT run_id || ' ' || seq || ' ' || kind || ' ' || left(payload::text, 300) FROM run_events ORDER BY created_at DESC LIMIT 40",
			"SELECT status || ':' || count(*) FROM dbos.workflow_status GROUP BY status",
		} {
			out, err := kube.SQL(ctx, src.in.Namespace, src.in.Release, q)
			section("source sql: "+q, out, err)
		}
	}
	if dst.in.Namespace != "" {
		for _, q := range []string{
			"SELECT workflow_uuid || ' ' || status || ' ' || created_at FROM dbos.workflow_status WHERE workflow_uuid LIKE 'trigger-fire:%' ORDER BY created_at",
			"SELECT status || ':' || count(*) FROM dbos.workflow_status GROUP BY status",
		} {
			out, err := kube.SQL(ctx, dst.in.Namespace, dst.in.Release, q)
			section("target sql: "+q, out, err)
		}
	}
	var ids []string
	for _, l := range rep.Labels() {
		if l != "step" {
			ids = append(ids, l)
		}
	}
	step := strings.Join(ids, "-")
	if step == "" {
		step = "suite"
	}
	_ = os.WriteFile(filepath.Join(outDir, "diag-"+step+".txt"), []byte(b.String()), 0o600)
})
