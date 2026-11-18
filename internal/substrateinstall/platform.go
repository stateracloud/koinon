// SPDX-License-Identifier: Apache-2.0

package substrateinstall

import (
	"context"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Platform is the kind of cluster the installer targets. The pinned ate-setup knows two routes: GKE
// (GCS, a Google service account and the GKE credential provider) and kind (an in-cluster object store,
// no cloud account). The kind route is mostly "not GCP", so Koinon uses it on any other Kubernetes
// flavor and corrects what is only right for a laptop (generic.go).
type Platform string

const (
	// PlatformGeneric is any Kubernetes cluster that is neither kind nor GKE (Talos, kubeadm, k3s, EKS and so
	// on). It uses ate-setup's kind route with Koinon's corrections and installs the gvisor sandbox class.
	PlatformGeneric Platform = "generic"
	// PlatformGKE is ate-setup's GKE route. It is never detected: ask for it.
	PlatformGKE Platform = "gke"
	// PlatformKind is a local kind cluster, with ate-setup's kind route as it is (docker on the installer host).
	PlatformKind Platform = "kind"
)

// Platforms lists the values of --platform.
var Platforms = []Platform{PlatformGeneric, PlatformGKE, PlatformKind}

// ParsePlatform reads the value of --platform. Empty means detect.
func ParsePlatform(s string) (Platform, error) {
	if s == "" {
		return "", nil
	}
	for _, p := range Platforms {
		if string(p) == s {
			return p, nil
		}
	}
	names := make([]string, len(Platforms))
	for n, p := range Platforms {
		names[n] = string(p)
	}
	return "", fmt.Errorf("unknown platform %q (valid: %s)", s, strings.Join(names, ", "))
}

// ErrPlatform is returned when the platform cannot be detected.
var ErrPlatform = errors.New("cannot detect the platform")

// DetectPlatform reads the nodes: kind when every node has a kind:// providerID, generic otherwise. It never
// detects gke: GKE needs a Google service account that the operator names, so it is a choice.
func (i *Installer) DetectPlatform(ctx context.Context) (Platform, error) {
	nodes, err := i.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("%w: cannot list nodes: %w; set --platform; nothing was installed", ErrPlatform, err)
	}
	if len(nodes.Items) == 0 {
		return "", fmt.Errorf("%w: the cluster has no nodes; nothing was installed", ErrPlatform)
	}
	for _, n := range nodes.Items {
		if !strings.HasPrefix(n.Spec.ProviderID, kindProviderPrefix) {
			return PlatformGeneric, nil
		}
	}
	return PlatformKind, nil
}

// validatePlatform refuses a platform that is unknown and the options that belong to another platform.
func (p Profile) validatePlatform() error {
	if p.Platform != "" {
		if _, err := ParsePlatform(string(p.Platform)); err != nil {
			return fmt.Errorf("substrate profile: %w", err)
		}
	}
	// An empty platform is detected later; Install validates the profile again with the detected one.
	if p.Platform != "" && p.Platform != PlatformGeneric {
		if name := firstSet(
			option{"--rustfs-storage-class", p.RustFSStorageClass},
			option{"--rustfs-storage-size", p.RustFSStorageSize},
			option{"--postgres-storage-size", p.PostgresStorageSize},
		); name != "" {
			return fmt.Errorf("substrate profile: %s is only for --platform generic (platform is %s)", name, p.Platform)
		}
	}
	if p.Platform != "" && p.Platform != PlatformGKE {
		if name := firstSet(
			option{"--gcp-project-id", p.GCPProjectID},
			option{"--snapshot-transfer-gsa-name", p.SnapshotTransferGSAName},
		); name != "" {
			return fmt.Errorf("substrate profile: %s is only for --platform gke (platform is %s)", name, p.Platform)
		}
	}
	if p.Platform == PlatformGeneric {
		return p.validateGeneric()
	}
	return nil
}

type option struct{ name, value string }

// firstSet is the name of the first option that has a value.
func firstSet(opts ...option) string {
	for _, o := range opts {
		if o.value != "" {
			return o.name
		}
	}
	return ""
}
