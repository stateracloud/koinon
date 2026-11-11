// SPDX-License-Identifier: Apache-2.0

package chartsmoke_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// dbSecretPassword is obviously fake, like every secret in the tests.
const dbSecretPassword = "koinon_test_not-a-real-password"

// wholeSecretKeys are the keys of a Secret that a pod mounts as a whole directory (no items),
// by volume name: the chart documents them in values.yaml.
var wholeSecretKeys = map[string][]string{
	"openshell-tls":     {"tls.crt", "tls.key", "ca.crt"},
	"otlp-token":        {"token"},
	"otlp-traces-token": {"token"},
}

// databaseKeys are the keys of the operator's database Secret, as values.yaml documents them
// (postgres.external.keys), not as the templates read them: a template that reads another key
// must fail.
// databaseKeyComponents maps each key of the operator's database Secret to the component it is for
// (postgres.external.keys: migrate, api, reconciler, agentd).
func databaseKeyComponents() map[string]string {
	b, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	must(err)
	var v struct {
		Postgres struct {
			External struct{ Keys map[string]string } `json:"external"`
		} `json:"postgres"`
	}
	must(yaml.Unmarshal(b, &v))
	out := map[string]string{}
	for component, key := range v.Postgres.External.Keys {
		out[key] = component
	}
	return out
}

func databaseKeys() []string {
	b, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	must(err)
	var v struct {
		Postgres struct {
			External struct{ Keys map[string]string } `json:"external"`
		} `json:"postgres"`
	}
	must(yaml.Unmarshal(b, &v))
	var keys []string
	for _, k := range v.Postgres.External.Keys {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		panic("values.yaml has no postgres.external.keys")
	}
	return keys
}

// podSpecs are the pod templates of every rendered Job and Deployment.
func (r *rendered) podSpecs() []corev1.PodSpec {
	var specs []corev1.PodSpec
	for _, j := range r.Jobs {
		specs = append(specs, j.Spec.Template.Spec)
	}
	for _, d := range r.Deployments {
		specs = append(specs, d.Spec.Template.Spec)
	}
	return specs
}

// generatedSecretNames are the Secrets the ensure-secrets hook Job creates, from its arguments.
func (r *rendered) generatedSecretNames() map[string]bool {
	names := map[string]bool{}
	for _, j := range r.Jobs {
		for _, c := range j.Spec.Template.Spec.Containers {
			for _, a := range c.Args {
				spec, ok := strings.CutPrefix(a, "--secrets=")
				if !ok {
					continue
				}
				var list []struct{ Name string }
				if err := json.Unmarshal([]byte(spec), &list); err != nil {
					panic(fmt.Sprintf("--secrets of %s: %v", j.Name, err))
				}
				for _, s := range list {
					names[s.Name] = true
				}
			}
		}
	}
	return names
}

// provisionOperatorSecrets creates, with fake data, the Secrets that the operator supplies
// before the install and that the chart neither renders nor has the ensure-secrets Job create:
// the database URIs (postgres.external.secretName, here pointing at the Postgres container), the
// OpenShell client certificate, an OIDC client secret, an existing KEK or bootstrap-admin Secret,
// the checkpoints' S3 credentials. Which Secrets and keys is read from the rendered pod specs,
// so a new operator Secret needs no change here, except for a Secret mounted as a whole
// directory (wholeSecretKeys).
func provisionOperatorSecrets(ctx context.Context, cl *cluster, r *rendered) []string {
	have := r.generatedSecretNames()
	for _, o := range r.Objects {
		if s, ok := o.(*corev1.Secret); ok {
			have[s.Name] = true
		}
	}
	// The migration role is the database owner. The other components connect as the login roles
	// the chart's CNPG cluster gives them (createComponentRoles), not as the owner, so a grant a
	// component needs and the chart does not give fails here as it would in an install.
	dsnFor := func(component string) string {
		user := "koinon"
		if component != "migrate" {
			user = "koinon_" + component
		}
		return fmt.Sprintf("postgres://%s:%s@postgres:5432/koinon?sslmode=disable", user, dbSecretPassword)
	}
	components := databaseKeyComponents()
	needs := map[string]map[string]string{}
	want := func(name, key, value string) {
		if have[name] {
			return
		}
		if needs[name] == nil {
			needs[name] = map[string]string{}
		}
		needs[name][key] = value
	}
	kekCreate := true
	// With a KMS the key-admin Job creates the KEK Secret (--kek-create), not koinon-api.
	jobCreatesKEK := false
	for _, ps := range r.podSpecs() {
		all := append(append([]corev1.Container{}, ps.InitContainers...), ps.Containers...)
		for _, c := range all {
			if slices.Contains(c.Args, "key-admin") && slices.Contains(c.Args, "--kek-create") {
				jobCreatesKEK = true
			}
			for _, e := range c.Env {
				if e.Name == "KOINON_KEK_CREATE" && e.Value == "false" {
					kekCreate = false
				}
				if ref := secretRef(e); ref != nil && (ref.Optional == nil || !*ref.Optional) {
					if e.Name == "DATABASE_URL" {
						// Every documented key, whichever one this container reads.
						for _, k := range databaseKeys() {
							want(ref.Name, k, dsnFor(components[k]))
						}
						continue
					}
					want(ref.Name, ref.Key, fakeValue(ref.Key))
				}
			}
		}
		for _, v := range ps.Volumes {
			s := v.Secret
			if s == nil || have[s.SecretName] || (s.Optional != nil && *s.Optional) {
				continue
			}
			keys := wholeSecretKeys[v.Name]
			for _, it := range s.Items {
				keys = append(keys, it.Key)
			}
			if len(keys) == 0 {
				panic(fmt.Sprintf("Secret %s is mounted as a whole directory (volume %s): add its keys to wholeSecretKeys", s.SecretName, v.Name))
			}
			for _, k := range keys {
				want(s.SecretName, k, fakeValue(k))
			}
		}
	}
	// An operator-managed KEK Secret (secrets.kek.existingSecret): koinon-api does not create it.
	if !kekCreate && !jobCreatesKEK {
		for _, ps := range r.podSpecs() {
			for _, v := range ps.Volumes {
				if v.Name == "kek" && v.Secret != nil && !have[v.Secret.SecretName] {
					id := "0123456789abcdef"
					key := make([]byte, 32)
					_, _ = rand.Read(key)
					for k, val := range map[string]string{id: base64.StdEncoding.EncodeToString(key), "active": id} {
						want(v.Secret.SecretName, k, val)
					}
				}
			}
		}
	}

	var names []string
	for name, kv := range needs {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name}, StringData: kv}
		if _, err := cl.Admin.CoreV1().Secrets(namespace).Create(ctx, s, metav1.CreateOptions{}); err != nil {
			panic(fmt.Sprintf("create operator Secret %s: %v", name, err))
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func secretRef(e corev1.EnvVar) *corev1.SecretKeySelector {
	if e.ValueFrom == nil {
		return nil
	}
	return e.ValueFrom.SecretKeyRef
}

var certPEM, keyPEM = selfSignedPEM()

// fakeValue is fake data for a Secret key: a certificate or key for the TLS keys, the shape of an
// S3 endpoint and region, else an obviously fake token.
func fakeValue(key string) string {
	switch key {
	case "tls.crt", "ca.crt":
		return certPEM
	case "tls.key":
		return keyPEM
	case "endpoint":
		return "http://s3.invalid:9000"
	case "region":
		return "us-east-1"
	case "password":
		return dbSecretPassword
	}
	// Long enough for a bearer token (32 characters).
	return "koinon_test_not-a-real-" + key + "-0123456789abcdef"
}

// selfSignedPEM is a throwaway self-signed certificate and its key.
func selfSignedPEM() (certPEM, keyPEM string) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(err)
	now := time.Now().Add(-time.Minute)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "koinon-chartsmoke-fake"},
		NotBefore: now, NotAfter: now.Add(24 * time.Hour), DNSNames: []string{"openshell.invalid"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	must(err)
	kder, err := x509.MarshalECPrivateKey(k)
	must(err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
}
