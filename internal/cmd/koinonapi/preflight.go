// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/preflight"
	"github.com/stateracloud/koinon/internal/project"
)

type preflightConfig struct {
	kubeconfig       string
	namespace        string
	timeout          time.Duration
	check            preflight.Config
	tlsDir           string
	supervisorImages string

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

// flags registers the flags of "koinon-api preflight".
func (c *preflightConfig) flags(fs *pflag.FlagSet) {
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config")
	fs.StringVar(&c.namespace, "namespace", "", "release namespace, where the seccomp probe pod runs")
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
	fs.StringVar(&c.check.AgentgatewayNamespace, "agentgateway-namespace", "agentgateway-system", "namespace of agentgateway's controller (kubernetes mode)")
	fs.StringVar(&c.check.AgentgatewayController, "agentgateway-controller", "agentgateway", "Deployment of agentgateway's controller (kubernetes mode)")
	fs.StringVar(&c.check.OpenShellConfigNamespace, "openshell-config-namespace", "openshell", "namespace of OpenShell's gateway ConfigMap")
	fs.StringVar(&c.check.OpenShellConfigMap, "openshell-config-map", "", "OpenShell's gateway ConfigMap (<fullname>-config); empty skips the sandbox image checks")
	fs.BoolVar(&c.check.ExpectSandboxPullSecrets, "expect-sandbox-pull-secrets", false, "the release pulls with credentials or through a mirror: warn when the OpenShell gateway has no sandbox pull secrets")
	fs.StringVar(&c.check.OpenShellGatewayID, "openshell-gateway-id", "", "openshell.gatewayId: warn when it differs from the OpenShell gateway's gateway_id (the sandbox observer's namespaces)")
	fs.BoolVar(&c.check.SandboxPolicyScopedToGateway, "sandbox-policy-scoped-to-gateway", false, "the sandbox admission policies bind by openshell.gatewayId: fail when it differs from the OpenShell gateway's gateway_id")
	fs.BoolVar(&c.check.SandboxObserverDisabled, "sandbox-observer-disabled", false, "sandboxObserver.enabled is false: warn that pull failures will show an unknown cause")
	fs.StringVar(&c.supervisorImages, "supervisor-images", "", "comma-separated sandboxPolicy.supervisorImages: warn when the OpenShell gateway's supervisor image is not one of them")
	fs.IntVar(&c.check.SupervisorTokenMaxSeconds, "supervisor-token-max-seconds", 0, "sandboxPolicy.supervisorTokenMaxSeconds: fail when the OpenShell gateway's supervisor token lifetime is above it")
	fs.BoolVar(&c.seccompProbe, "seccomp-probe", true, "run a pod with the sandbox RuntimeClass to check Kata guest seccomp")
	fs.StringVar(&c.probeImage, "probe-image", "", "image of the seccomp probe pod (this binary's image)")
	fs.StringVar(&c.probePullSecrets, "probe-image-pull-secrets", "", "comma-separated image pull Secrets of the probe pod")
	fs.DurationVar(&c.probeTimeout, "probe-timeout", 2*time.Minute, "how long the seccomp probe may take, image pull included")
	fs.StringVar(&c.probeNamePrefix, "probe-name-prefix", "koinon-preflight-probe", "name prefix of the probe pod")
	fs.StringVar(&c.probeInstance, "probe-instance", "", "app.kubernetes.io/instance label of the probe pods")
	fs.BoolVar(&c.netpolProbe, "netpol-probe", true, "run two pods and a NetworkPolicy to check that NetworkPolicy is enforced")
	fs.DurationVar(&c.netpolTimeout, "netpol-probe-timeout", 90*time.Second, "how long the NetworkPolicy probe may take, image pulls included")
	fs.StringVar(&c.netpolNamePrefix, "netpol-probe-name-prefix", "koinon-preflight-netpol", "name prefix of the NetworkPolicy probe's objects")
}

// validate fills what the flags imply and checks the rules between them.
func (c *preflightConfig) validate() error {
	c.check.OpenShellPlaintext = c.tlsDir == ""
	c.check.SupervisorImages = splitList(c.supervisorImages)
	if c.seccompProbe && (c.probeImage == "" || c.namespace == "") {
		return errors.New("--seccomp-probe needs --probe-image and --namespace (or POD_NAMESPACE)")
	}
	if c.netpolProbe && (c.probeImage == "" || c.namespace == "") {
		return errors.New("--netpol-probe needs --probe-image and --namespace (or POD_NAMESPACE)")
	}
	return nil
}

func newPreflightCommand() *cobra.Command {
	var c preflightConfig
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Check the cluster for Koinon's install prerequisites",
		Long: "Checks the cluster for Koinon's install prerequisites and exits non-zero when one is missing (plan item H2); " +
			"the chart runs it as a pre-install and pre-upgrade hook Job. It needs no database. " +
			"Its subcommands are what the probe pods run.",
		Args:    cobra.NoArgs,
		PreRunE: func(*cobra.Command, []string) error { return c.validate() },
		RunE:    func(cmd *cobra.Command, _ []string) error { return runPreflight(cmd.Context(), c, cmd.OutOrStdout()) },
	}
	c.flags(cmd.Flags())
	cli.EnvAlias(cmd, "namespace", "POD_NAMESPACE")

	cmd.AddCommand(&cobra.Command{
		Use:   "probe",
		Short: "Run inside the seccomp probe pod: report what the guest kernel allows",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := preflight.SelfProbe()
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), res)
			return err
		},
	}, &cobra.Command{
		Use:   "netpol-server",
		Short: "Run inside the NetworkPolicy probe's server pod",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return preflight.ServeNetPolProbe(cmd.Context(), "") },
	}, newNetPolClientCommand())
	return cmd
}

// newNetPolClientCommand is "preflight netpol-client", the NetworkPolicy probe's client pod.
func newNetPolClientCommand() *cobra.Command {
	var c preflight.NetPolClient
	cmd := &cobra.Command{
		Use:   "netpol-client",
		Short: "Run inside the NetworkPolicy probe's client pod",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), c.Run(cmd.Context()))
			return err
		},
	}
	cmd.Flags().StringVar(&c.Host, "target", "", "IP of the probe server pod")
	cmd.Flags().DurationVar(&c.Window, "window", 5*time.Second, "how long the denied port must keep answering to count as open")
	mustRequire(cmd, "target")
	return cmd
}

// runPreflight is "koinon-api preflight": the install checks.
func runPreflight(ctx context.Context, cfg preflightConfig, out io.Writer) error {
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
