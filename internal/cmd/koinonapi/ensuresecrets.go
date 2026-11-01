// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/ensuresecrets"
)

// newEnsureSecretsCommand is "koinon-api ensure-secrets", the chart's pre-install and pre-upgrade
// hook Job: it creates the Secrets with generated passwords that do not exist yet and leaves the
// others alone (internal/ensuresecrets).
func newEnsureSecretsCommand() *cobra.Command {
	var secrets, namespace, kubeconfig string
	cmd := &cobra.Command{
		Use:   "ensure-secrets",
		Short: "Create the Secrets with generated passwords that do not exist yet",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEnsureSecrets(cmd.Context(), secrets, namespace, kubeconfig, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&secrets, "secrets", "", `JSON list of the Secrets to ensure: [{"name":"..","type":"..","username":"..","labels":{..}}]`)
	cmd.Flags().StringVar(&namespace, "namespace", "", "namespace of the Secrets")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config")
	cli.EnvAlias(cmd, "namespace", "POD_NAMESPACE")
	mustRequire(cmd, "secrets", "namespace")
	return cmd
}

func runEnsureSecrets(ctx context.Context, secrets, namespace, kubeconfig string, out io.Writer) error {
	if namespace == "" {
		return errors.New("--namespace (or $POD_NAMESPACE) is required")
	}
	var specs []ensuresecrets.Spec
	if err := json.Unmarshal([]byte(secrets), &specs); err != nil {
		return fmt.Errorf("--secrets: %w", err)
	}
	rc, err := restConfig(kubeconfig)
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	_, err = ensuresecrets.Ensure(ctx, client, namespace, specs, out)
	return err
}
