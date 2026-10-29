// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// Container is the agentgateway container in the pod, default "agentgateway" (the chart's
	// name). A pod with a sidecar (service mesh, log shipper) needs it: the log API refuses a
	// request that names no container.
	Container string
}

// defaultGatewayContainer is the chart's agentgateway container name.
const defaultGatewayContainer = "agentgateway"

var _ ReloadLog = PodLogReload{}

// ReloadFailure implements ReloadLog.
func (p PodLogReload) LatestReloadFailure(ctx context.Context, after time.Time) (ReloadFailure, bool, error) {
	dep, err := p.Client.AppsV1().Deployments(p.Namespace).Get(ctx, p.Deployment, metav1.GetOptions{})
	if err != nil {
		return ReloadFailure{}, false, fmt.Errorf("read the gateway Deployment %s: %w", p.Deployment, err)
	}
	if dep.Spec.Selector == nil {
		return ReloadFailure{}, false, fmt.Errorf("the gateway Deployment %s has no selector", p.Deployment)
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return ReloadFailure{}, false, fmt.Errorf("the gateway Deployment %s selector: %w", p.Deployment, err)
	}
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
		opts := &corev1.PodLogOptions{Container: cmp.Or(p.Container, defaultGatewayContainer)}
		if !after.IsZero() {
			// Bounds the read to what can matter (callers never pass zero, see lookback); the API
			// takes whole seconds, so ParseReloadFailure filters exactly.
			t := metav1.NewTime(after.Add(-time.Second))
			opts.SinceTime = &t
		}
		stream, err := p.Client.CoreV1().Pods(p.Namespace).GetLogs(pod.Name, opts).Stream(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("read the log of %s: %w", pod.Name, err))
			continue
		}
		f, ok, err := ParseReloadFailure(stream, after)
		_ = stream.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("read the log of %s: %w", pod.Name, err))
		}
		if ok && (!found || f.At.After(best.At)) {
			best, found = f, true
		}
	}
	if !found && len(errs) > 0 {
		return ReloadFailure{}, false, errs[0]
	}
	return best, found, nil
}

// maxLogLine is the longest log line ParseReloadFailure parses; a longer one (a huge access-log
// entry) is skipped whole, and the scan goes on after it.
const maxLogLine = 1 << 20

// ParseReloadFailure scans agentgateway's JSON log for the latest failed reload logged strictly
// after the given time. Lines over maxLogLine are skipped; a read error ends the scan and is
// returned with whatever was found before it.
func ParseReloadFailure(r io.Reader, after time.Time) (f ReloadFailure, found bool, err error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	skipping := false
	for {
		chunk, rerr := br.ReadSlice('\n')
		switch {
		case errors.Is(rerr, bufio.ErrBufferFull):
			// A partial line: collect it up to maxLogLine, then drop the rest of it.
			if !skipping {
				line = append(line, chunk...)
				skipping = len(line) > maxLogLine
			}
			continue
		case rerr != nil && !errors.Is(rerr, io.EOF):
			return f, found, rerr
		}
		if !skipping {
			line = append(line, chunk...)
			if cand, ok := reloadFailureIn(line, after); ok && (!found || !cand.At.Before(f.At)) {
				f, found = cand, true
			}
		}
		line, skipping = line[:0], false
		if rerr != nil { // EOF
			return f, found, nil
		}
	}
}

// reloadFailureIn parses one log line; ok is false unless it is a failed reload after the time.
func reloadFailureIn(line []byte, after time.Time) (ReloadFailure, bool) {
	if !bytes.Contains(line, []byte(reloadFailedMessage)) {
		return ReloadFailure{}, false
	}
	var e struct {
		Time    time.Time `json:"time"`
		Message string    `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil {
		return ReloadFailure{}, false
	}
	msg, ok := strings.CutPrefix(e.Message, reloadFailedMessage)
	if !ok || !e.Time.After(after) {
		return ReloadFailure{}, false
	}
	return ReloadFailure{At: e.Time, Reason: truncate(strings.TrimSpace(msg), maxReasonLen)}, true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "..."
}
