// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/stateracloud/koinon/internal/project"
)

// The NetworkPolicy probe. Koinon's chart restricts who may reach the OTLP receiver and
// agentgateway with NetworkPolicies, which only work when the cluster's network plugin enforces
// them, and Koinon must not depend on a particular plugin (AGENTS.md). So the check is
// functional: a server pod listens on two ports, a NetworkPolicy selecting only that pod allows
// ingress to one of them (the control port) and so denies the other (the denied port), and a
// client pod connects to both. The control port proves the client can reach the server at all;
// the denied port then answers only if the policy is not enforced.
//
// Both pods run Koinon's own image ("koinon-api preflight netpol-server" and "netpol-client")
// with the cluster's default runtime, in the release namespace, unprivileged and without a
// service account token.

// NetPolMarker starts the client's output line and is the server's answer.
const NetPolMarker = "koinon-netpol-probe"

// Ports of the NetworkPolicy probe server.
const (
	NetPolDeniedPort  = 8080
	NetPolControlPort = 8081
)

// NetPolResult is what the probe client saw.
type NetPolResult struct {
	// ControlReached is whether the client reached the port the policy allows.
	ControlReached bool
	// DeniedReached is whether the client still reached the port the policy denies, on every
	// attempt for the whole window.
	DeniedReached bool
}

// String formats the result as the client prints it.
func (r NetPolResult) String() string {
	control, denied := "unreachable", "untested"
	switch {
	case r.ControlReached && r.DeniedReached:
		control, denied = "ok", "open"
	case r.ControlReached:
		control, denied = "ok", "blocked"
	}
	return fmt.Sprintf("%s control=%s denied=%s", NetPolMarker, control, denied)
}

// ParseNetPolOutput reads the client's line from its pod log.
func ParseNetPolOutput(log string) (NetPolResult, error) {
	for line := range strings.Lines(log) {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != NetPolMarker {
			continue
		}
		var control, denied string
		for _, f := range fields[1:] {
			k, v, _ := strings.Cut(f, "=")
			switch k {
			case "control":
				control = v
			case "denied":
				denied = v
			}
		}
		switch control + "/" + denied {
		case "ok/open":
			return NetPolResult{ControlReached: true, DeniedReached: true}, nil
		case "ok/blocked":
			return NetPolResult{ControlReached: true}, nil
		case "unreachable/untested":
			return NetPolResult{}, nil
		}
		return NetPolResult{}, fmt.Errorf("malformed probe output %q", strings.TrimSpace(line))
	}
	return NetPolResult{}, fmt.Errorf("no %q line in the probe client's log: %q", NetPolMarker, truncate(log, 200))
}

// listenNetPolProbe opens every probe port before anything answers. Opening them all first
// matters: a client that reaches the first port must find the others listening too, or it reads
// a port that is merely not yet open as blocked. addr is the host to listen on, "" for all.
func listenNetPolProbe(ctx context.Context, addr string, ports ...int) ([]net.Listener, error) {
	if len(ports) == 0 {
		ports = []int{NetPolDeniedPort, NetPolControlPort}
	}
	var lc net.ListenConfig
	var listeners []net.Listener
	for _, port := range ports {
		l, err := lc.Listen(ctx, "tcp", net.JoinHostPort(addr, strconv.Itoa(port)))
		if err != nil {
			for _, o := range listeners {
				_ = o.Close()
			}
			return nil, err
		}
		listeners = append(listeners, l)
	}
	return listeners, nil
}

// ServeNetPolProbe is what the server pod runs: it answers NetPolMarker on both probe ports
// until ctx ends. addr is the host to listen on, "" for all.
func ServeNetPolProbe(ctx context.Context, addr string, ports ...int) error {
	listeners, err := listenNetPolProbe(ctx, addr, ports...)
	if err != nil {
		return err
	}
	return ServeNetPolProbeOn(ctx, listeners...)
}

// ServeNetPolProbeOn answers NetPolMarker on the listeners until ctx ends, and closes them.
func ServeNetPolProbeOn(ctx context.Context, listeners ...net.Listener) error {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, NetPolMarker+"\n")
	})
	errs := make(chan error, len(listeners))
	var servers []*http.Server
	for _, l := range listeners {
		srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
		servers = append(servers, srv)
		go func() { errs <- srv.Serve(l) }()
	}
	closeAll := func() {
		for _, s := range servers {
			_ = s.Close()
		}
	}
	select {
	case <-ctx.Done():
		closeAll()
		return nil
	case err := <-errs:
		closeAll()
		return err
	}
}

// NetPolClient is what the client pod runs.
type NetPolClient struct {
	// Host is the server pod's IP.
	Host string
	// ControlPort and DeniedPort default to NetPolControlPort and NetPolDeniedPort.
	ControlPort, DeniedPort int
	// ControlWait is how long the client retries the control port, for the server to start
	// listening (default 30s).
	ControlWait time.Duration
	// Window is how long the denied port must keep answering to count as open (default 5s):
	// a plugin may take a moment to apply a new policy.
	Window time.Duration
	// Attempt bounds each connection (default 1s); Pause is the time between attempts
	// (default 500ms).
	Attempt, Pause time.Duration
}

// Run probes the server and returns what it saw.
func (c NetPolClient) Run(ctx context.Context) NetPolResult {
	controlPort := orInt(c.ControlPort, NetPolControlPort)
	deniedPort := orInt(c.DeniedPort, NetPolDeniedPort)
	pause := orDuration(c.Pause, 500*time.Millisecond)
	// A new connection per attempt: a kept-alive one could predate the policy.
	client := &http.Client{
		Timeout:   orDuration(c.Attempt, time.Second),
		Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil},
	}
	reach := func(port int) bool {
		url := "http://" + net.JoinHostPort(c.Host, strconv.Itoa(port)) + "/"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
		return err == nil && strings.HasPrefix(string(body), NetPolMarker)
	}
	wait := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(pause):
			return true
		}
	}

	var res NetPolResult
	deadline := time.Now().Add(orDuration(c.ControlWait, 30*time.Second))
	for !res.ControlReached {
		res.ControlReached = reach(controlPort)
		if !res.ControlReached && (time.Now().After(deadline) || !wait()) {
			return res
		}
	}
	end := time.Now().Add(orDuration(c.Window, 5*time.Second))
	for {
		if !reach(deniedPort) {
			return res
		}
		if time.Now().After(end) || !wait() {
			res.DeniedReached = true
			return res
		}
	}
}

func orInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

// NetPolProber runs the NetworkPolicy probe.
type NetPolProber interface {
	Probe(ctx context.Context) (NetPolResult, error)
}

// KubeNetPolProber runs the NetworkPolicy probe with a server pod, a client pod and a
// NetworkPolicy in the release namespace, and deletes all three when it returns, also on a
// timeout. The client pod and the policy are also owned by the server pod, so the garbage
// collector removes them should an explicit delete fail.
type KubeNetPolProber struct {
	Client           kubernetes.Interface
	Namespace        string
	NamePrefix       string
	Image            string
	ImagePullSecrets []string
	Labels           map[string]string
	// Timeout bounds the whole probe, image pulls included (default 90s).
	Timeout time.Duration
	// PollInterval is how often the pods' phases are read (default 2s).
	PollInterval time.Duration
	// Window is passed to the client (NetPolClient.Window).
	Window time.Duration
	// Logs reads a pod's log; nil reads it through Client.
	Logs func(ctx context.Context, namespace, name string) (string, error)
	// Token names the objects and labels the server pod; empty generates a random one.
	Token string
}

// netpolTokenLabel selects the probe's server pod in its NetworkPolicy.
var netpolTokenLabel = project.Domain + "/preflight-netpol-probe"

func (p KubeNetPolProber) timeout() time.Duration { return orDuration(p.Timeout, 90*time.Second) }

func (p KubeNetPolProber) names() (token, server, client, policy string) {
	token = p.Token
	if token == "" {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		token = hex.EncodeToString(b)
	}
	return token, p.NamePrefix + "-server-" + token, p.NamePrefix + "-client-" + token, p.NamePrefix + "-" + token
}

// pod is a probe pod: the cluster's default runtime, restricted Pod Security, no token.
func (p KubeNetPolProber) pod(name, component string, labels map[string]string, args []string, ports []corev1.ContainerPort) *corev1.Pod {
	var pullSecrets []corev1.LocalObjectReference
	for _, s := range p.ImagePullSecrets {
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: s})
	}
	all := maps.Clone(p.Labels)
	if all == nil {
		all = map[string]string{}
	}
	maps.Copy(all, labels)
	all["app.kubernetes.io/component"] = component
	deadline := int64(p.timeout().Seconds())
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.Namespace, Labels: all},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  ptr.To(false),
			EnableServiceLinks:            ptr.To(false),
			ActiveDeadlineSeconds:         &deadline,
			TerminationGracePeriodSeconds: ptr.To[int64](0),
			ImagePullSecrets:              pullSecrets,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To[int64](65532),
				RunAsGroup:     ptr.To[int64](65532),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  "probe",
				Image: p.Image,
				Args:  args,
				Ports: ports,
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
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			}},
		},
	}
}

// ServerPod is the probe's server pod.
func (p KubeNetPolProber) ServerPod(name, token string) *corev1.Pod {
	return p.pod(name, "preflight-netpol-server", map[string]string{netpolTokenLabel: token},
		[]string{"preflight", "netpol-server"},
		[]corev1.ContainerPort{
			{Name: "denied", ContainerPort: NetPolDeniedPort, Protocol: corev1.ProtocolTCP},
			{Name: "control", ContainerPort: NetPolControlPort, Protocol: corev1.ProtocolTCP},
		})
}

// ClientPod is the probe's client pod, connecting to the server at host.
func (p KubeNetPolProber) ClientPod(name, host string) *corev1.Pod {
	return p.pod(name, "preflight-netpol-client", nil,
		[]string{"preflight", "netpol-client", "--target=" + host, "--window=" + orDuration(p.Window, 5*time.Second).String()},
		nil)
}

// Policy is the probe's NetworkPolicy: ingress to the server pod only on the control port.
func (p KubeNetPolProber) Policy(name, token string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.Namespace, Labels: maps.Clone(p.Labels)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{netpolTokenLabel: token}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				Ports: []networkingv1.NetworkPolicyPort{{
					Protocol: ptr.To(corev1.ProtocolTCP),
					Port:     ptr.To(intstr.FromInt32(NetPolControlPort)),
				}},
			}},
		},
	}
}

// Probe creates the server pod and the policy, waits for the server to run, runs the client
// and reads its log. Every object it created is deleted before it returns.
func (p KubeNetPolProber) Probe(ctx context.Context) (NetPolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	token, serverName, clientName, policyName := p.names()
	pods := p.Client.CoreV1().Pods(p.Namespace)
	policies := p.Client.NetworkingV1().NetworkPolicies(p.Namespace)
	cleanup := func(del func(context.Context) error) {
		delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = del(delCtx)
	}
	deletePod := func(name string) func(context.Context) error {
		return func(ctx context.Context) error {
			return pods.Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
		}
	}

	server, err := pods.Create(ctx, p.ServerPod(serverName, token), metav1.CreateOptions{})
	if err != nil {
		return NetPolResult{}, fmt.Errorf("create probe server pod: %w", err)
	}
	defer cleanup(deletePod(serverName))
	owner := []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: server.Name, UID: server.UID}}

	policy := p.Policy(policyName, token)
	policy.OwnerReferences = owner
	if _, err := policies.Create(ctx, policy, metav1.CreateOptions{}); err != nil {
		return NetPolResult{}, fmt.Errorf("create probe NetworkPolicy: %w", err)
	}
	defer cleanup(func(ctx context.Context) error { return policies.Delete(ctx, policyName, metav1.DeleteOptions{}) })

	server, err = p.waitFor(ctx, server, func(pod *corev1.Pod) (bool, error) {
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return pod.Status.PodIP != "", nil
		case corev1.PodSucceeded, corev1.PodFailed:
			return false, fmt.Errorf("probe server pod %s ended %s", pod.Name, pod.Status.Phase)
		}
		return false, nil
	})
	if err != nil {
		return NetPolResult{}, err
	}

	client := p.ClientPod(clientName, server.Status.PodIP)
	client.OwnerReferences = owner
	created, err := pods.Create(ctx, client, metav1.CreateOptions{})
	if err != nil {
		return NetPolResult{}, fmt.Errorf("create probe client pod: %w", err)
	}
	defer cleanup(deletePod(clientName))
	if _, err := p.waitFor(ctx, created, func(pod *corev1.Pod) (bool, error) {
		return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed, nil
	}); err != nil {
		return NetPolResult{}, err
	}
	readLogs := p.Logs
	if readLogs == nil {
		readLogs = p.podLogs
	}
	log, err := readLogs(ctx, p.Namespace, clientName)
	if err != nil {
		return NetPolResult{}, fmt.Errorf("read probe client pod %s log: %w", clientName, err)
	}
	return ParseNetPolOutput(log)
}

// waitFor reads pod until done says so, ctx ends or done fails.
func (p KubeNetPolProber) waitFor(ctx context.Context, pod *corev1.Pod, done func(*corev1.Pod) (bool, error)) (*corev1.Pod, error) {
	interval := orDuration(p.PollInterval, 2*time.Second)
	name := pod.Name
	for {
		ok, err := done(pod)
		if err != nil {
			return nil, err
		}
		if ok {
			return pod, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("probe pod %s still %s after %s%s", name, pod.Status.Phase, p.timeout(), waitingReason(pod))
		case <-time.After(interval):
		}
		next, err := p.Client.CoreV1().Pods(p.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("probe pod %s still %s after %s%s", name, pod.Status.Phase, p.timeout(), waitingReason(pod))
			}
			return nil, fmt.Errorf("read probe pod %s: %w", name, err)
		}
		pod = next
	}
}

func (p KubeNetPolProber) podLogs(ctx context.Context, namespace, name string) (string, error) {
	rc, err := p.Client.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{Container: "probe"}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, 64<<10))
	return string(b), err
}
