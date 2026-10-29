// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// reloadFailedMessage is the prefix of agentgateway's log message for a config it refused
// (v1.7.0 agentgateway::state_manager, level error).
const reloadFailedMessage = "Failed to reload config: "

// maxReasonLen caps the reason: it goes into logs, a metric-adjacent message and agents' status.
const maxReasonLen = 512

// PodLogReload reads agentgateway's reload errors from the logs of the gateway pods (the
// Deployment's selector), which is the only place agentgateway reports them. The reconciler needs
// get on the Deployment, list on pods and get on pods/log in its namespace (chart rbac.yaml).
type PodLogReload struct {
	Client     kubernetes.Interface
	Namespace  string
	Deployment string
}

var _ ReloadLog = PodLogReload{}

// ReloadFailure implements ReloadLog.
func (p PodLogReload) LatestReloadFailure(ctx context.Context, after time.Time) (ReloadFailure, bool, error) {
	dep, err := p.Client.AppsV1().Deployments(p.Namespace).Get(ctx, p.Deployment, metav1.GetOptions{})
	if err != nil {
		return ReloadFailure{}, false, fmt.Errorf("read the gateway Deployment %s: %w", p.Deployment, err)
	}
	sel := labels.SelectorFromSet(dep.Spec.Selector.MatchLabels)
	pods, err := p.Client.CoreV1().Pods(p.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return ReloadFailure{}, false, fmt.Errorf("list the gateway pods: %w", err)
	}
	var (
		best  ReloadFailure
		found bool
		errs  []error
	)
	for _, pod := range pods.Items {
		opts := &corev1.PodLogOptions{}
		if !after.IsZero() {
			// The API takes whole seconds; ParseReloadFailure filters exactly.
			t := metav1.NewTime(after.Add(-time.Second))
			opts.SinceTime = &t
		}
		stream, err := p.Client.CoreV1().Pods(p.Namespace).GetLogs(pod.Name, opts).Stream(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("read the log of %s: %w", pod.Name, err))
			continue
		}
		f, ok := ParseReloadFailure(stream, after)
		_ = stream.Close()
		if ok && (!found || f.At.After(best.At)) {
			best, found = f, true
		}
	}
	if !found && len(errs) > 0 {
		return ReloadFailure{}, false, errs[0]
	}
	return best, found, nil
}

// ParseReloadFailure scans agentgateway's JSON log for the latest failed reload logged strictly
// after the given time.
func ParseReloadFailure(r io.Reader, after time.Time) (f ReloadFailure, found bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), reloadFailedMessage) {
			continue
		}
		var e struct {
			Time    time.Time `json:"time"`
			Message string    `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		msg, ok := strings.CutPrefix(e.Message, reloadFailedMessage)
		if !ok || !e.Time.After(after) {
			continue
		}
		if !found || !e.Time.Before(f.At) {
			f, found = ReloadFailure{At: e.Time, Reason: truncate(strings.TrimSpace(msg), maxReasonLen)}, true
		}
	}
	return f, found
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "..."
}
