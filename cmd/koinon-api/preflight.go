// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/procoduck/koinon/internal/cli"
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

	netpolProbe      bool
	netpolTimeout    time.Duration
	netpolNamePrefix string
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
	fs.StringVar(&c.probeInstance, "probe-instance", "", "app.kubernetes.io/instance label of the probe pods")
	fs.BoolVar(&c.netpolProbe, "netpol-probe", true, "run two pods and a NetworkPolicy to check that NetworkPolicy is enforced")
	fs.DurationVar(&c.netpolTimeout, "netpol-probe-timeout", 90*time.Second, "how long the NetworkPolicy probe may take, image pulls included")
	fs.StringVar(&c.netpolNamePrefix, "netpol-probe-name-prefix", "koinon-preflight-netpol", "name prefix of the NetworkPolicy probe's objects")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if c.seccompProbe && (c.probeImage == "" || c.namespace == "") {
		return c, errors.New("--seccomp-probe needs --probe-image and --namespace (or POD_NAMESPACE)")
	}
	if c.netpolProbe && (c.probeImage == "" || c.namespace == "") {
		return c, errors.New("--netpol-probe needs --probe-image and --namespace (or POD_NAMESPACE)")
	}
	return c, nil
}

// runNetPolClient is "preflight netpol-client", the NetworkPolicy probe's client pod.
func runNetPolClient(ctx context.Context, args []string, out io.Writer) error {
	var c preflight.NetPolClient
	fs := flag.NewFlagSet("koinon-api preflight netpol-client", flag.ContinueOnError)
	fs.StringVar(&c.Host, "target", "", "IP of the probe server pod")
	fs.DurationVar(&c.Window, "window", 5*time.Second, "how long the denied port must keep answering to count as open")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.Host == "" || fs.NArg() > 0 {
		return errors.New("netpol-client needs --target and no other arguments")
	}
	_, err := fmt.Fprintln(out, c.Run(ctx))
	return err
}

// runPreflight is "koinon-api preflight": the install checks, or what the probe pods run:
// "preflight probe" (seccomp), "preflight netpol-server" and "preflight netpol-client".
func runPreflight(ctx context.Context, args []string, out io.Writer) error {
	switch {
	case len(args) == 1 && args[0] == "probe":
		res, err := preflight.SelfProbe()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, res)
		return err
	case len(args) == 1 && args[0] == "netpol-server":
		return preflight.ServeNetPolProbe(ctx, "")
	case len(args) > 0 && args[0] == "netpol-client":
		return runNetPolClient(ctx, args[1:], out)
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
		gw, err := cli.OpenShell{Address: cfg.check.OpenShellAddress, TLSDir: cfg.tlsDir}.Dial()
		if err != nil {
			return err
		}
		defer func() { _ = gw.Close() }()
		checker.OpenShell = gw
	}
	labels := map[string]string{
		"app.kubernetes.io/name":      "koinon",
		"app.kubernetes.io/component": "preflight-probe",
		"app.kubernetes.io/part-of":   "koinon",
	}
	if cfg.probeInstance != "" {
		labels["app.kubernetes.io/instance"] = cfg.probeInstance
	}
	if cfg.netpolProbe {
		checker.NetPol = preflight.KubeNetPolProber{
			Client:           kube,
			Namespace:        cfg.namespace,
			NamePrefix:       cfg.netpolNamePrefix,
			Image:            cfg.probeImage,
			ImagePullSecrets: splitList(cfg.probePullSecrets),
			Labels:           labels,
			Timeout:          cfg.netpolTimeout,
		}
	}
	if cfg.seccompProbe {
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
