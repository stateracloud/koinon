// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// The guest seccomp probe (ADR 0048). Spike 8.16 showed that a plain, unprivileged pod with the
// Kata RuntimeClass and seccompProfile RuntimeDefault reads Seccomp: 2 in /proc/self/status
// when Kata applies guest seccomp and 0 when it does not. An OpenShell sandbox cannot tell the
// two apart this way (OpenShell's own filter always sets Seccomp: 2), so the probe runs a plain
// pod and also calls keyctl, which RuntimeDefault denies and OpenShell's filter allows (8.12).
// The probe pod runs Koinon's own image ("koinon-api preflight probe"), so it needs no
// other image.

// ProbeMarker starts the probe's output line.
const ProbeMarker = "koinon-seccomp-probe"

// ProbeResult is what the probe saw from inside the pod.
type ProbeResult struct {
	// Seccomp is the Seccomp field of /proc/self/status: 0 off, 2 filtering.
	Seccomp int
	// Filters is Seccomp_filters, or -1 when the kernel does not report it.
	Filters int
	// Keyctl is the outcome of keyctl(KEYCTL_GET_KEYRING_ID): EPERM when seccomp denied it,
	// "allowed" when it succeeded, or another errno name.
	Keyctl string
}

// String formats the result as the probe prints it.
func (r ProbeResult) String() string {
	return fmt.Sprintf("%s seccomp=%d filters=%d keyctl=%s", ProbeMarker, r.Seccomp, r.Filters, r.Keyctl)
}

// ParseProbeOutput reads the probe line from the probe pod's log.
func ParseProbeOutput(log string) (ProbeResult, error) {
	for line := range strings.Lines(log) {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != ProbeMarker {
			continue
		}
		res := ProbeResult{Seccomp: -1, Filters: -1}
		for _, f := range fields[1:] {
			k, v, _ := strings.Cut(f, "=")
			switch k {
			case "seccomp":
				res.Seccomp, _ = strconv.Atoi(v)
			case "filters":
				res.Filters, _ = strconv.Atoi(v)
			case "keyctl":
				res.Keyctl = v
			}
		}
		if res.Seccomp < 0 || res.Keyctl == "" {
			return ProbeResult{}, fmt.Errorf("malformed probe output %q", strings.TrimSpace(line))
		}
		return res, nil
	}
	return ProbeResult{}, fmt.Errorf("no %q line in the probe pod's log: %q", ProbeMarker, truncate(log, 200))
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// parseStatus reads Seccomp and Seccomp_filters from /proc/self/status.
func parseStatus(status string) (seccomp, filters int, err error) {
	seccomp, filters = -1, -1
	for line := range strings.Lines(status) {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch k {
		case "Seccomp":
			seccomp, err = strconv.Atoi(strings.TrimSpace(v))
		case "Seccomp_filters":
			filters, err = strconv.Atoi(strings.TrimSpace(v))
		}
		if err != nil {
			return -1, -1, fmt.Errorf("parse %s: %w", k, err)
		}
	}
	if seccomp < 0 {
		return -1, -1, errors.New("no Seccomp field in /proc/self/status")
	}
	return seccomp, filters, nil
}

// KubeProber runs the probe as a pod in the release namespace and reads its log.
type KubeProber struct {
	Client           kubernetes.Interface
	Namespace        string
	NamePrefix       string
	Image            string
	ImagePullSecrets []string
	RuntimeClassName string
	Labels           map[string]string
	// Timeout bounds the whole probe, including the image pull.
	Timeout time.Duration
	// PollInterval is how often the pod's phase is read (default 2s).
	PollInterval time.Duration
	// Logs reads the pod's log; nil reads it through Client.
	Logs func(ctx context.Context, namespace, name string) (string, error)
}

// Pod is the probe pod: unprivileged, restricted Pod Security, no token, no network use.
func (p KubeProber) Pod() *corev1.Pod {
	var pullSecrets []corev1.LocalObjectReference
	for _, s := range p.ImagePullSecrets {
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: s})
	}
	deadline := int64(p.timeout().Seconds())
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: p.NamePrefix + "-",
			Namespace:    p.Namespace,
			Labels:       p.Labels,
		},
		Spec: corev1.PodSpec{
			RuntimeClassName:             ptr.To(p.RuntimeClassName),
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr.To(false),
			EnableServiceLinks:           ptr.To(false),
			ActiveDeadlineSeconds:        &deadline,
			ImagePullSecrets:             pullSecrets,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To[int64](65532),
				RunAsGroup:     ptr.To[int64](65532),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  "probe",
				Image: p.Image,
				Args:  []string{"preflight", "probe"},
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot:             ptr.To(true),
					AllowPrivilegeEscalation: ptr.To(false),
					ReadOnlyRootFilesystem:   ptr.To(true),
					Privileged:               ptr.To(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("16Mi"),
					},
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			}},
		},
	}
}

func (p KubeProber) timeout() time.Duration {
	if p.Timeout <= 0 {
		return 2 * time.Minute
	}
	return p.Timeout
}

// Probe creates the probe pod, waits for it to finish, reads its log and deletes it.
func (p KubeProber) Probe(ctx context.Context) (ProbeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	pods := p.Client.CoreV1().Pods(p.Namespace)
	pod, err := pods.Create(ctx, p.Pod(), metav1.CreateOptions{})
	if err != nil {
		return ProbeResult{}, fmt.Errorf("create probe pod: %w", err)
	}
	name := pod.Name
	defer func() {
		delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = pods.Delete(delCtx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
	}()

	interval := p.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		select {
		case <-ctx.Done():
			return ProbeResult{}, fmt.Errorf("probe pod %s still %s after %s%s", name, pod.Status.Phase, p.timeout(), waitingReason(pod))
		case <-time.After(interval):
		}
		if pod, err = pods.Get(ctx, name, metav1.GetOptions{}); err != nil {
			return ProbeResult{}, fmt.Errorf("read probe pod %s: %w", name, err)
		}
	}
	readLogs := p.Logs
	if readLogs == nil {
		readLogs = p.podLogs
	}
	log, err := readLogs(ctx, p.Namespace, name)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("read probe pod %s log: %w", name, err)
	}
	return ParseProbeOutput(log)
}

func (p KubeProber) podLogs(ctx context.Context, namespace, name string) (string, error) {
	rc, err := p.Client.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{Container: "probe"}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, 64<<10))
	return string(b), err
}

// waitingReason explains why a pod has not started, for the timeout message.
func waitingReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			return fmt.Sprintf(" (%s: %s)", w.Reason, truncate(w.Message, 200))
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return fmt.Sprintf(" (unschedulable: %s)", truncate(c.Message, 200))
		}
	}
	return ""
}
