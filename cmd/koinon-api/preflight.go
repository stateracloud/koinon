// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/procoduck/koinon/internal/execution/openshell"
	"github.com/procoduck/koinon/internal/preflight"
	"github.com/procoduck/koinon/internal/project"
)

type preflightConfig struct {
	kubeconfig string
	namespace  string
	timeout    time.Duration
	check      preflight.Config
	tlsDir     string

	seccompProbe     bool
	probeImage       string
	probePullSecrets string
	probeTimeout     time.Duration
	probeNamePrefix  string
	probeInstance    string
}

func parsePreflightFlags(args []string) (preflightConfig, error) {
	var c preflightConfig
	fs := flag.NewFlagSet("koinon-api preflight", flag.ContinueOnError)
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config")
	fs.StringVar(&c.namespace, "namespace", os.Getenv("POD_NAMESPACE"), "release namespace, where the seccomp probe pod runs")
	fs.DurationVar(&c.timeout, "call-timeout", 15*time.Second, "timeout of each Kubernetes API and gateway call")
	c.check.PostgresMode, c.check.GatewayMode = project.PostgresCNPG, project.GatewayStandalone
	fs.Var(&c.check.PostgresMode, "postgres-mode", "postgres.mode: cnpg or external")
	fs.Var(&c.check.GatewayMode, "gateway-mode", "gateway.mode: standalone or kubernetes")
	fs.StringVar(&c.check.GatewayClassName, "gateway-class", "agentgateway", "GatewayClass of agentgateway in kubernetes mode")
	fs.StringVar(&c.check.RuntimeClassName, "runtime-class", "kata", "sandbox RuntimeClass")
	fs.StringVar(&c.check.OpenShellAddress, "openshell-address", "", "OpenShell gateway host:port")
	fs.StringVar(&c.tlsDir, "openshell-tls-dir", "", "directory with the OpenShell client tls.crt, tls.key and ca.crt; empty is plaintext")
	fs.StringVar(&c.check.AgentSandboxNamespace, "agent-sandbox-namespace", "agent-sandbox-system", "namespace of the Agent Sandbox controller")
	fs.StringVar(&c.check.AgentSandboxController, "agent-sandbox-controller", "agent-sandbox-controller", "Deployment of the Agent Sandbox controller")
	fs.BoolVar(&c.seccompProbe, "seccomp-probe", true, "run a pod with the sandbox RuntimeClass to check Kata guest seccomp")
	fs.StringVar(&c.probeImage, "probe-image", "", "image of the seccomp probe pod (this binary's image)")
	fs.StringVar(&c.probePullSecrets, "probe-image-pull-secrets", "", "comma-separated image pull Secrets of the probe pod")
	fs.DurationVar(&c.probeTimeout, "probe-timeout", 2*time.Minute, "how long the seccomp probe may take, image pull included")
	fs.StringVar(&c.probeNamePrefix, "probe-name-prefix", "koinon-preflight-probe", "name prefix of the probe pod")
	fs.StringVar(&c.probeInstance, "probe-instance", "", "app.kubernetes.io/instance label of the probe pod")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if c.seccompProbe && (c.probeImage == "" || c.namespace == "") {
		return c, errors.New("--seccomp-probe needs --probe-image and --namespace (or POD_NAMESPACE)")
	}
	return c, nil
}

// runPreflight is "koinon-api preflight": the install checks, or, as "preflight probe", the
// seccomp probe that runs inside the probe pod.
func runPreflight(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 1 && args[0] == "probe" {
		res, err := preflight.SelfProbe()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, res)
		return err
	}
	cfg, err := parsePreflightFlags(args)
	if err != nil {
		return err
	}
	rc, err := restConfig(cfg.kubeconfig)
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return err
	}
	checker := &preflight.Checker{Kube: kube, Dynamic: dyn, Config: cfg.check, CallTimeout: cfg.timeout}
	if cfg.check.OpenShellAddress != "" {
		gw, closeGW, err := dialGateway(cfg.check.OpenShellAddress, cfg.tlsDir)
		if err != nil {
			return err
		}
		defer closeGW()
		checker.OpenShell = gw
	}
	if cfg.seccompProbe {
		labels := map[string]string{
			"app.kubernetes.io/name":      "koinon",
			"app.kubernetes.io/component": "preflight-probe",
			"app.kubernetes.io/part-of":   "koinon",
		}
		if cfg.probeInstance != "" {
			labels["app.kubernetes.io/instance"] = cfg.probeInstance
		}
		checker.Prober = preflight.KubeProber{
			Client:           kube,
			Namespace:        cfg.namespace,
			NamePrefix:       cfg.probeNamePrefix,
			Image:            cfg.probeImage,
			ImagePullSecrets: splitList(cfg.probePullSecrets),
			RuntimeClassName: cfg.check.RuntimeClassName,
			Labels:           labels,
			Timeout:          cfg.probeTimeout,
		}
	}
	if failed := preflight.Write(out, checker.Run(ctx)); failed > 0 {
		return fmt.Errorf("%w: %d checks", preflight.ErrFailed, failed)
	}
	return nil
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	var (
		rc  *rest.Config
		err error
	)
	if kubeconfig != "" {
		rc, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		rc, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("kubernetes client config: %w", err)
	}
	return rc, nil
}

func dialGateway(address, tlsDir string) (preflight.OpenShell, func(), error) {
	if tlsDir != "" {
		b, err := openshell.Dial(openshell.Config{
			Endpoint: address,
			CertFile: filepath.Join(tlsDir, "tls.crt"),
			KeyFile:  filepath.Join(tlsDir, "tls.key"),
			CAFile:   filepath.Join(tlsDir, "ca.crt"),
		})
		if err != nil {
			return nil, nil, err
		}
		return b, func() { _ = b.Close() }, nil
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("connect to the OpenShell gateway: %w", err)
	}
	return openshell.New(conn), func() { _ = conn.Close() }, nil
}
