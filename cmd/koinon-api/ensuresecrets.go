// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"k8s.io/client-go/kubernetes"

	"github.com/stateracloud/koinon/internal/ensuresecrets"
)

// runEnsureSecrets runs "koinon-api ensure-secrets", the chart's pre-install and pre-upgrade hook
// Job: it creates the Secrets with generated passwords that do not exist yet and leaves the others
// alone (internal/ensuresecrets).
func runEnsureSecrets(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("koinon-api ensure-secrets", flag.ContinueOnError)
	var secrets, namespace, kubeconfig string
	fs.StringVar(&secrets, "secrets", "", `JSON list of the Secrets to ensure: [{"name":"..","type":"..","username":"..","labels":{..}}]`)
	fs.StringVar(&namespace, "namespace", os.Getenv("POD_NAMESPACE"), "namespace of the Secrets")
	fs.StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config")
	if err := fs.Parse(args); err != nil {
		return err
	}
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
