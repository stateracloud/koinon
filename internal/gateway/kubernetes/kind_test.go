// SPDX-License-Identifier: Apache-2.0

package kubernetes_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/gatewaytest"
	gwk8s "github.com/stateracloud/koinon/internal/gateway/kubernetes"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// The kind suite needs a cluster with the Gateway API CRDs and agentgateway v1.7.0 (hack/g2-kind.sh
// creates one, runs this package and deletes it). KOINON_KIND_KUBECONFIG points at its kubeconfig;
// without it the specs skip. KOINON_KIND_HOST is the address pods use to reach the test host.
const (
	envKubeconfig = "KOINON_KIND_KUBECONFIG"
	envHost       = "KOINON_KIND_HOST"

	providersSecret = "koinon-gateway-providers"
	gatewayName     = "koinon-gateway"
)

type cluster struct {
	cfgPath string
	kube    kubernetes.Interface
	dyn     dynamic.Interface
	host    string
}

var (
	clusterOnce sync.Once
	theCluster  *cluster
)

// needCluster returns the kind cluster or skips the spec.
func needCluster() *cluster {
	testenv.Require(
		testenv.Var("g2-kind", envKubeconfig),
		testenv.Var("g2-kind", envHost),
	) // hack/g2-kind.sh sets both
	clusterOnce.Do(func() {
		path := os.Getenv(envKubeconfig)
		host := os.Getenv(envHost)
		rc, err := clientcmd.BuildConfigFromFlags("", path)
		Expect(err).NotTo(HaveOccurred())
		// The driver applies a dozen objects per pass; client-go's default 5 requests per second
		// would make every Apply take seconds.
		rc.QPS, rc.Burst = 100, 200
		kube, err := kubernetes.NewForConfig(rc)
		Expect(err).NotTo(HaveOccurred())
		dyn, err := dynamic.NewForConfig(rc)
		Expect(err).NotTo(HaveOccurred())
		theCluster = &cluster{cfgPath: path, kube: kube, dyn: dyn, host: host}
	})
	return theCluster
}

// kubeGateway is Koinon's Gateway in a fresh namespace of the kind cluster, reached through a
// local TCP forwarder that survives pod replacement.
type kubeGateway struct {
	c      *cluster
	ns     string
	driver *gwk8s.Driver
	store  gwk8s.DynamicStore
	ln     net.Listener
	URL    string

	mu  sync.Mutex
	pf  *exec.Cmd
	pfa string
}

var nsCounter = struct {
	sync.Mutex
	n int
}{}

func startKube(ctx context.Context, hostPorts []int, files map[string]string, opts gwk8s.Options) (*kubeGateway, error) {
	c := needCluster()
	nsCounter.Lock()
	nsCounter.n++
	n := nsCounter.n
	nsCounter.Unlock()
	ns := fmt.Sprintf("g2-%d-%d", time.Now().Unix()%100000, n)
	if _, err := c.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		return nil, err
	}
	data := map[string][]byte{}
	for k, v := range files {
		data[k] = []byte(v)
	}
	if _, err := c.kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: providersSecret}, Data: data,
	}, metav1.CreateOptions{}); err != nil {
		return nil, err
	}
	g := &kubeGateway{c: c, ns: ns}
	var err error
	if g.ln, err = (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0"); err != nil {
		return nil, err
	}
	g.URL = "http://" + g.ln.Addr().String()
	go g.serve()

	opts.Namespace, opts.GatewayName, opts.ProviderKeysSecret = ns, gatewayName, providersSecret
	g.store = gwk8s.DynamicStore{Client: c.dyn}
	g.driver = &gwk8s.Driver{
		Log:     slog.New(slog.NewTextHandler(GinkgoWriter, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Options: opts,
		Store:   g.store,
		Events:  c.kube,
		Waiter:  standalone.ProbeWaiter{GatewayURL: g.URL, Interval: 100 * time.Millisecond, MaxInterval: time.Second},
		// The first Apply creates the proxy: allow for its image pull and start.
		StatusTimeout:  2 * time.Minute,
		StatusInterval: 100 * time.Millisecond,
	}
	return g, nil
}

// serve forwards each connection to the proxy through a kubectl port-forward to the Service.
func (g *kubeGateway) serve() {
	for {
		in, err := g.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = in.Close() }()
			var out net.Conn
			var err error
			for range 3 {
				addr, perr := g.forward()
				if perr == nil {
					if out, err = (&net.Dialer{Timeout: 3 * time.Second}).DialContext(context.Background(), "tcp", addr); err == nil {
						break
					}
				}
				g.resetForward()
				time.Sleep(500 * time.Millisecond)
			}
			if out == nil {
				return
			}
			defer func() { _ = out.Close() }()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(out, in); done <- struct{}{} }()
			go func() { _, _ = io.Copy(in, out); done <- struct{}{} }()
			<-done
		}()
	}
}

var forwardLine = regexp.MustCompile(`Forwarding from (127\.0\.0\.1:\d+)`)

// forward returns the local address of a live kubectl port-forward, starting one when needed.
func (g *kubeGateway) forward() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pf != nil && g.pf.ProcessState == nil && g.pfa != "" {
		return g.pfa, nil
	}
	cmd := exec.CommandContext(context.Background(), "kubectl", "--kubeconfig", g.c.cfgPath, "-n", g.ns, "port-forward", "--address", "127.0.0.1", "svc/"+gatewayName, ":4000")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() { _ = cmd.Wait() }()
	addr := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if m := forwardLine.FindStringSubmatch(sc.Text()); m != nil {
				addr <- m[1]
				break
			}
		}
		_, _ = io.Copy(io.Discard, out)
	}()
	select {
	case a := <-addr:
		g.pf, g.pfa = cmd, a
		return a, nil
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		return "", fmt.Errorf("kubectl port-forward did not start")
	}
}

func (g *kubeGateway) resetForward() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pf != nil {
		_ = g.pf.Process.Kill()
	}
	g.pf, g.pfa = nil, ""
}

func (g *kubeGateway) stop(ctx context.Context) error {
	_ = g.ln.Close()
	g.resetForward()
	err := g.c.kube.CoreV1().Namespaces().Delete(ctx, g.ns, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// logs returns the proxy's log, for failure reports.
func (g *kubeGateway) logs(ctx context.Context) string {
	pods, err := g.c.kube.CoreV1().Pods(g.ns).List(ctx, metav1.ListOptions{LabelSelector: "gateway.networking.k8s.io/gateway-name=" + gatewayName})
	if err != nil {
		return err.Error()
	}
	var b strings.Builder
	for _, p := range pods.Items {
		rc, err := g.c.kube.CoreV1().Pods(g.ns).GetLogs(p.Name, &corev1.PodLogOptions{TailLines: ptrInt64(200)}).Stream(ctx)
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(rc)
		_ = rc.Close()
		b.Write(raw)
	}
	return b.String()
}

func ptrInt64(n int64) *int64 { return &n }

func (g *kubeGateway) proxyPods(ctx context.Context) []corev1.Pod {
	pods, err := g.c.kube.CoreV1().Pods(g.ns).List(ctx, metav1.ListOptions{LabelSelector: "gateway.networking.k8s.io/gateway-name=" + gatewayName})
	Expect(err).NotTo(HaveOccurred())
	return pods.Items
}

var _ = func() bool {
	gatewaytest.Register(gatewaytest.Env{
		Name:      "kubernetes",
		KnownGaps: gatewaytest.AgentgatewayV170Gaps,
		Host:      func() string { return os.Getenv(envHost) }(),
		KeyFile:   func(name string) string { return "/provider-keys/" + name },
		// The key stage answers before any response policy runs (old issue 342).
		Unmarked: []string{gatewaytest.RefusalLLMKey, gatewaytest.RefusalMCPKey},
		Start: func(ctx context.Context, ports []int, files map[string]string, guardURL string) (*gatewaytest.Gateway, error) {
			g, err := startKube(ctx, ports, files, gwk8s.Options{GuardURL: guardURL})
			if err != nil {
				return nil, err
			}
			// Create the Gateway and wait for the proxy; the specs Apply their state next.
			if _, err := g.driver.Apply(ctx, gateway.State{}); err != nil {
				_ = g.stop(ctx)
				return nil, err
			}
			return &gatewaytest.Gateway{Driver: g.driver, URL: g.URL, Logs: g.logs, Stop: g.stop}, nil
		},
	})
	return true
}()
