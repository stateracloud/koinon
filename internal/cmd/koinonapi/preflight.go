// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
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
	kubeContext      string
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

	driver                   string
	substratePools           []string
	substrateNodeSelector    []string
	substrateVMTier          bool
	substrateGVisor          bool
	substrateProbes          bool
	substrateNamespace       string
	substrateWorkerNamespace string
	substrateProbeNamespace  string
	substrateIssuer          string
	substrateRouterAddress   string
	substrateRouterService   string
	substrateAgentdNS        string
	substrateAgentdSA        string

	netpolProbe      bool
	netpolTimeout    time.Duration
	netpolNamePrefix string
}

// flags registers the flags of "koinon-api preflight".
func (c *preflightConfig) flags(fs *pflag.FlagSet) {
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config")
	fs.StringVar(&c.kubeContext, "context", "", "kubeconfig context; empty uses the kubeconfig's current context (needs --kubeconfig)")
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
	fs.StringVar(&c.check.OpenShellWorkload, "openshell-workload", "", "OpenShell gateway StatefulSet (or Deployment), read for OPENSHELL_K8S_SA_TOKEN_TTL_SECS")
	fs.BoolVar(&c.check.ExpectSandboxPullSecrets, "expect-sandbox-pull-secrets", false, "the release pulls with credentials or through a mirror: warn when the OpenShell gateway has no sandbox pull secrets")
	fs.StringVar(&c.check.OpenShellGatewayID, "openshell-gateway-id", "", "openshell.gatewayId: warn when it differs from the OpenShell gateway's gateway_id (the sandbox observer's namespaces)")
	fs.BoolVar(&c.check.SandboxPolicyScopedToGateway, "sandbox-policy-scoped-to-gateway", false, "the sandbox admission policies bind by openshell.gatewayId: fail when it differs from the OpenShell gateway's gateway_id")
	fs.BoolVar(&c.check.SandboxObserverDisabled, "sandbox-observer-disabled", false, "sandboxObserver.enabled is false: warn that pull failures will show an unknown cause")
	fs.StringVar(&c.supervisorImages, "supervisor-images", "", "comma-separated sandboxPolicy.supervisorImages: warn when the OpenShell gateway's supervisor image is not one of them")
	fs.IntVar(&c.check.SupervisorTokenMaxSeconds, "supervisor-token-max-seconds", 0, "sandboxPolicy.supervisorTokenMaxSeconds: fail when the OpenShell gateway's supervisor token lifetime is above it")
	fs.StringVar(&c.driver, "driver", "openshell", "execution.driver: openshell or substrate (substrate replaces the OpenShell and Agent Sandbox checks)")
	fs.StringVar(&c.substrateNamespace, "substrate-namespace", "ate-system", "namespace of Agent Substrate")
	fs.StringVar(&c.substrateWorkerNamespace, "substrate-worker-namespace", "ate-workers", "namespace of the WorkerPools (execution.substrate.workerNamespace)")
	fs.StringVar(&c.substrateProbeNamespace, "substrate-probe-namespace", preflight.DefaultSubstrateProbeNamespace,
		"namespace of the Substrate probe pods, never a Substrate namespace (their admission policies refuse a bare pod); created with Pod Security privileged when missing, and then removed")
	fs.StringArrayVar(&c.substratePools, "substrate-worker-pool", nil, "<tier>=<WorkerPool name> that must be ready (repeatable)")
	fs.StringArrayVar(&c.substrateNodeSelector, "substrate-node-selector", nil, "<key>=<value> limiting the device probe to the worker nodes (repeatable)")
	fs.BoolVar(&c.substrateVMTier, "substrate-vm-tier", true, "the vm tier is in use: probe a node for /dev/kvm, /dev/net/tun, /dev/vhost-vsock and /dev/vhost-net")
	fs.BoolVar(&c.substrateGVisor, "substrate-gvisor", true, "the gVisor sandbox class is in use: probe each worker node for user.max_user_namespaces above 0 (gVisor starts its sentry in a user namespace)")
	fs.StringVar(&c.substrateRouterAddress, "substrate-router-address", "", "execution.substrate.routerAddress: the router Service must serve its port (empty skips)")
	fs.StringVar(&c.substrateRouterService, "substrate-router-service", "", "the router Service (default atenet-router)")
	fs.StringVar(&c.substrateAgentdNS, "substrate-agentd-namespace", "", "namespace of Koinon's agentd: the router allow-list must hold its identity (with --substrate-agentd-service-account)")
	fs.StringVar(&c.substrateAgentdSA, "substrate-agentd-service-account", "", "ServiceAccount of Koinon's agentd, whose pod certificate the router must allow (empty skips the check)")
	fs.StringVar(&c.substrateIssuer, "substrate-expected-issuer", "", "the service-account issuer Substrate trusts (ate-setup --ateapi-expected-jwt-issuer); empty is the issuer in the API server's discovery document")
	fs.BoolVar(&c.substrateProbes, "substrate-probes", true, "run the issuer probe pod, the device probe pod, the user-namespace probe pods and the /var/lib/ate filesystem probe pods in --substrate-probe-namespace")
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
	switch c.driver {
	case "openshell":
	case "substrate":
		sub := preflight.SubstrateConfig{
			Namespace: c.substrateNamespace, WorkerNamespace: c.substrateWorkerNamespace, ProbeNamespace: c.substrateProbeNamespace,
			RouterService: c.substrateRouterService, AgentdNamespace: c.substrateAgentdNS, AgentdServiceAccount: c.substrateAgentdSA,
			ExpectedIssuer: c.substrateIssuer, VMTier: c.substrateVMTier, GVisor: c.substrateGVisor, WorkerPools: map[string]string{},
		}
		if c.substrateRouterAddress != "" {
			_, port, err := net.SplitHostPort(c.substrateRouterAddress)
			if err != nil {
				return fmt.Errorf("--substrate-router-address %q: %w", c.substrateRouterAddress, err)
			}
			if sub.RouterPort, err = strconv.Atoi(port); err != nil || sub.RouterPort < 1 || sub.RouterPort > 65535 {
				return fmt.Errorf("--substrate-router-address %q: port %q is not a port number", c.substrateRouterAddress, port)
			}
		}
		if (c.substrateAgentdSA == "") != (c.substrateAgentdNS == "") {
			return errors.New("--substrate-agentd-namespace and --substrate-agentd-service-account go together")
		}
		if err := sub.CheckProbeNamespace(); err != nil {
			return fmt.Errorf("--substrate-probe-namespace: %w", err)
		}
		for _, kv := range c.substratePools {
			tier, name, ok := strings.Cut(kv, "=")
			if !ok || tier == "" || name == "" {
				return fmt.Errorf("--substrate-worker-pool %q: want <tier>=<name>", kv)
			}
			sub.WorkerPools[tier] = name
		}
		if len(c.substrateNodeSelector) > 0 {
			sub.NodeSelector = map[string]string{}
		}
		for _, kv := range c.substrateNodeSelector {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return fmt.Errorf("--substrate-node-selector %q: want <key>=<value>", kv)
			}
			sub.NodeSelector[k] = v
		}
		c.check.Substrate = &sub
		c.seccompProbe = false // the guest seccomp probe is Kata's (ADR 0048); the launcher reports its own controls (ADR 0120)
		if c.substrateProbes && c.probeImage == "" {
			return errors.New("--substrate-probes needs --probe-image")
		}
	default:
		return fmt.Errorf("--driver %q: want openshell or substrate", c.driver)
	}
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
	}, newNetPolClientCommand(), newIssuerProbeCommand(), &cobra.Command{
		Use:   "userns-probe",
		Short: "Run inside the Substrate user-namespace probe pod: report the node's user.max_user_namespaces",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			n, err := preflight.ReadMaxUserNamespaces(preflight.MaxUserNamespacesPath)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), preflight.UserNamespacesLine(n))
			return err
		},
	}, &cobra.Command{
		Use:   "atefs-probe",
		Short: "Run inside the Substrate /var/lib/ate probe pod: report whether /var/lib/ate shares the filesystem of the node root or of kubelet",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			shared, missing, err := preflight.SharedFilesystems()
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), preflight.AteFSLine(shared, missing))
			return err
		},
	}, &cobra.Command{
		Use:   "devices-probe",
		Short: "Run inside the Substrate device probe pod: report that the node's devices were mounted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), preflight.SubstrateDevicesProbeMarker, "ok")
			return err
		},
	})
	return cmd
}

// newIssuerProbeCommand is "preflight issuer-probe", the Substrate issuer probe pod.
func newIssuerProbeCommand() *cobra.Command {
	var issuer string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "issuer-probe",
		Short: "Run inside the Substrate issuer probe pod: fetch the OIDC discovery document and JWKS the way Substrate's api server does",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), preflight.ProbeIssuerFromPod(cmd.Context(), issuer, timeout))
			return err
		},
	}
	cmd.Flags().StringVar(&issuer, "issuer", "", "the service-account token issuer URL")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "how long the fetch may take")
	mustRequire(cmd, "issuer")
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
	checker, _, closeGateway, err := buildChecker(cfg)
	if err != nil {
		return err
	}
	defer closeGateway()
	if sub := cfg.check.Substrate; sub != nil && cfg.substrateProbes {
		// The probe pods run in a namespace of their own (issue #1176). One that this run creates, it removes.
		ns := preflight.ProbeNamespace{Client: checker.Kube, Name: sub.WithDefaults().ProbeNamespace}
		created, err := ns.Ensure(ctx)
		if err != nil {
			return err
		}
		if created {
			defer func() {
				if err := ns.Remove(ctx); err != nil {
					_, _ = fmt.Fprintln(out, err)
				}
			}()
		}
		if err := ns.WaitReady(ctx); err != nil {
			return err
		}
	}
	if failed := preflight.Write(out, checker.Run(ctx)); failed > 0 {
		return fmt.Errorf("%w: %d checks", preflight.ErrFailed, failed)
	}
	return nil
}

// buildChecker wires the preflight's Kubernetes clients and probes from the flags. The returned
// function closes the OpenShell connection.
func buildChecker(cfg preflightConfig) (*preflight.Checker, kubernetes.Interface, func(), error) {
	noop := func() {}
	rc, err := restConfigContext(cfg.kubeconfig, cfg.kubeContext)
	if err != nil {
		return nil, nil, noop, err
	}
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, nil, noop, err
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, nil, noop, err
	}
	checker := &preflight.Checker{Kube: kube, Dynamic: dyn, Config: cfg.check, CallTimeout: cfg.timeout}
	if sub := cfg.check.Substrate; sub != nil && cfg.substrateProbes {
		prober := substrateProber(cfg, kube, *sub)
		checker.Issuer, checker.IssuerProbe, checker.Devices, checker.UserNS = preflight.NewIssuerResolver(kube), prober, prober, prober
		checker.AteFS = prober
	}
	if cfg.check.OpenShellAddress != "" && cfg.check.Substrate == nil {
		gw, err := cli.OpenShell{Address: cfg.check.OpenShellAddress, TLSDir: cfg.tlsDir}.Dial()
		if err != nil {
			return nil, nil, noop, err
		}
		noop = func() { _ = gw.Close() }
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
	return checker, kube, noop, nil
}

// restConfigContext is restConfig with a kubeconfig context: the named one, not the file's current one.
func restConfigContext(kubeconfig, kubeContext string) (*rest.Config, error) {
	if kubeContext == "" {
		return restConfig(kubeconfig)
	}
	if kubeconfig == "" {
		return nil, errors.New("--context needs --kubeconfig")
	}
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes client config (context %s): %w", kubeContext, err)
	}
	return rc, nil
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

// substrateProber builds the prober of the Substrate preflight. Its pods run in the probe namespace, never in a
// Substrate namespace: Substrate's admission policies refuse a bare pod there (issue #1176).
func substrateProber(cfg preflightConfig, kube kubernetes.Interface, sub preflight.SubstrateConfig) preflight.KubeSubstrateProber {
	return preflight.KubeSubstrateProber{
		Client:           kube,
		Namespace:        sub.WithDefaults().ProbeNamespace,
		NamePrefix:       cfg.probeNamePrefix,
		Image:            cfg.probeImage,
		ImagePullSecrets: splitList(cfg.probePullSecrets),
		NodeSelector:     sub.NodeSelector,
		Timeout:          cfg.probeTimeout,
		Labels: map[string]string{
			"app.kubernetes.io/name": "koinon", "app.kubernetes.io/part-of": "koinon",
		},
	}
}
