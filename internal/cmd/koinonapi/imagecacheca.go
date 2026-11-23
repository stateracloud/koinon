// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"errors"
	"io"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/imagecacheca"
)

// newImageCacheCACommand is "koinon-api imagecache-ca", the chart's pre-install and pre-upgrade hook Job that
// comes after ensure-secrets: it publishes the public CA of the image cache to the Substrate namespace, so atelet
// trusts the cache, and rolls atelet and the cache when the certificates changed (internal/imagecacheca, issue #1191).
func newImageCacheCACommand() *cobra.Command {
	var p imagecacheca.Params
	var kubeconfig string
	cmd := &cobra.Command{
		Use:   "imagecache-ca",
		Short: "Publish the public CA of the image cache to the Substrate namespace",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runImageCacheCA(cmd.Context(), p, kubeconfig, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&p.Secret, "tls-secret", "", "TLS Secret of the image cache (with ca.crt) in the release namespace")
	cmd.Flags().StringVar(&p.Namespace, "namespace", "", "release namespace")
	cmd.Flags().StringVar(&p.SubstrateNamespace, "substrate-namespace", "", "namespace of Substrate (ate-system)")
	cmd.Flags().StringVar(&p.Deployment, "cache-deployment", "", "Deployment of the image cache in the release namespace; empty skips its restart stamp")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config")
	cli.EnvAlias(cmd, "namespace", "POD_NAMESPACE")
	mustRequire(cmd, "tls-secret", "namespace", "substrate-namespace")
	return cmd
}

func runImageCacheCA(ctx context.Context, p imagecacheca.Params, kubeconfig string, out io.Writer) error {
	if p.Namespace == "" {
		return errors.New("--namespace (or $POD_NAMESPACE) is required")
	}
	rc, err := restConfig(kubeconfig)
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	return imagecacheca.Publish(ctx, client, p, out)
}
