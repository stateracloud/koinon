// SPDX-License-Identifier: Apache-2.0

package substrateinstall

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/stateracloud/koinon/internal/preflight"
)

// Substrate's api server reads its trusted issuers from the ConfigMap ate-api-authentication, key
// authentication.yaml, in its namespace (cmd/ate-setup/internal/steps/create.go ConfigMapAPIAuthn and
// applyAPIAuthenticationConfig; internal/apiconfig/authn.go for the schema).
const (
	authConfigMap = "ate-api-authentication"
	authConfigKey = "authentication.yaml"
	// apiServerName is Substrate's api server Deployment.
	apiServerName = "ate-api-server"
	// authRestartAnnotation makes the api server read a changed ConfigMap: ate-setup does the same with its env hash.
	authRestartAnnotation = "koinon.dev/authentication-config-hash"
	// kubernetesProvider is the name of the entry ate-setup writes (kubernetesJWTProvider).
	kubernetesProvider = "kubernetes"
	// apiAudience is the audience ate-setup gives that entry.
	apiAudience = "api.ate-system.svc"
)

// authConfig and jwtProvider mirror Substrate's apiconfig.AuthenticationConfig and JWTProviderConfig (the json
// tags), with every other provider kept as it is.
type authConfig struct {
	JWTProviders []map[string]any `json:"jwtProviders"`
}

// EnsureAPIAuthentication makes Substrate's api server trust issuer with the pod's own CA and token when
// issuer is the cluster's own (https://kubernetes.default.svc): a discovery request to that address needs
// both, and a provider without them fails with "invalid bearer token". It writes the entry that Substrate
// v1.0.0 ate-setup writes (kubernetesJWTProvider) and restarts the api server only when it changed the
// ConfigMap. For any other issuer it does nothing: the system roots verify the server and no credential is sent.
//
// Interim: Koinon must not depend on how ate-setup was run. Remove this when the pinned Substrate commit
// writes the entry itself for every install path; exit condition and tracking in
// https://github.com/stateracloud/koinon/issues/1108.
func (i *Installer) EnsureAPIAuthentication(ctx context.Context, issuer string) (changed bool, err error) {
	if !preflight.IsInClusterIssuer(issuer) {
		return false, nil
	}
	ns := i.Checker.Config.SubstrateOrDefault().Namespace
	cms := i.Kube.CoreV1().ConfigMaps(ns)
	want := map[string]any{
		"name": kubernetesProvider, "issuer": issuer, "audiences": []any{apiAudience},
		"certificateAuthorityFile": preflight.IssuerCAFile, "discoveryTokenFile": preflight.IssuerTokenFile,
	}
	cm, err := cms.Get(ctx, authConfigMap, metav1.GetOptions{})
	var cfg authConfig
	switch {
	case apierrors.IsNotFound(err):
		cm = nil
	case err != nil:
		return false, fmt.Errorf("read ConfigMap %s/%s: %w", ns, authConfigMap, err)
	default:
		if err := yaml.Unmarshal([]byte(cm.Data[authConfigKey]), &cfg); err != nil {
			return false, fmt.Errorf("parse %s of ConfigMap %s/%s: %w", authConfigKey, ns, authConfigMap, err)
		}
	}
	// Reconcile the entry that already uses this issuer, whatever its name: Substrate's validator rejects two
	// providers with one issuer. Else the entry named kubernetes. Else add one.
	idx := -1
	for n, p := range cfg.JWTProviders {
		if p["issuer"] == issuer {
			idx = n
			break
		}
	}
	if idx < 0 {
		for n, p := range cfg.JWTProviders {
			if p["name"] == kubernetesProvider {
				idx = n
				break
			}
		}
	}
	if idx < 0 {
		cfg.JWTProviders = append([]map[string]any{want}, cfg.JWTProviders...)
	} else {
		p := cfg.JWTProviders[idx]
		a, _ := p["audiences"].([]any)
		if p["issuer"] == issuer && p["certificateAuthorityFile"] == preflight.IssuerCAFile && p["discoveryTokenFile"] == preflight.IssuerTokenFile && len(a) > 0 {
			// Already right. The ConfigMap may still have been written by a run whose restart failed.
			return i.restartIfStale(ctx, ns, cm.Data[authConfigKey])
		}
		// Keep the entry's name and audiences (an operator may have added one); an empty list is invalid, so it gets the default.
		if n, ok := p["name"].(string); ok && n != "" {
			want["name"] = n
		}
		if len(a) > 0 {
			want["audiences"] = a
		}
		cfg.JWTProviders[idx] = want
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return false, fmt.Errorf("marshal the authentication config: %w", err)
	}
	data := strings.TrimRight(string(out), "\n")
	if cm == nil {
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: authConfigMap, Namespace: ns},
			Data:       map[string]string{authConfigKey: data},
		}, metav1.CreateOptions{})
	} else {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[authConfigKey] = data
		_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	}
	if err != nil {
		return false, fmt.Errorf("write ConfigMap %s/%s: %w", ns, authConfigMap, err)
	}
	return true, i.restartAPIServer(ctx, ns, data)
}

// restartAPIServer stamps the new config on the api server's pod template, which rolls it. A missing
// Deployment is fine: nothing runs yet, and the first start reads the ConfigMap.
func (i *Installer) restartAPIServer(ctx context.Context, ns, data string) error {
	deps := i.Kube.AppsV1().Deployments(ns)
	d, err := deps.Get(ctx, apiServerName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Deployment %s/%s: %w", ns, apiServerName, err)
	}
	stampAuthConfig(d, data)
	if _, err := deps.Update(ctx, d, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("restart Deployment %s/%s: %w", ns, apiServerName, err)
	}
	return nil
}

// restartIfStale rolls the api server when its pod template does not carry the hash of data: a rerun after a
// write whose restart failed finds the ConfigMap right and the Deployment still on the old config.
func (i *Installer) restartIfStale(ctx context.Context, ns, data string) (bool, error) {
	d, err := i.Kube.AppsV1().Deployments(ns).Get(ctx, apiServerName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read Deployment %s/%s: %w", ns, apiServerName, err)
	}
	if d.Spec.Template.Annotations[authRestartAnnotation] == fmt.Sprintf("%x", sha256.Sum256([]byte(data))) {
		return false, nil
	}
	return true, i.restartAPIServer(ctx, ns, data)
}

func stampAuthConfig(d *appsv1.Deployment, data string) {
	if d.Spec.Template.Annotations == nil {
		d.Spec.Template.Annotations = map[string]string{}
	}
	d.Spec.Template.Annotations[authRestartAnnotation] = fmt.Sprintf("%x", sha256.Sum256([]byte(data)))
}
