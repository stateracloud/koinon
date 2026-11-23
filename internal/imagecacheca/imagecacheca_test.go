// SPDX-License-Identifier: Apache-2.0

package imagecacheca_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/imagecacheca"
)

const (
	relNS = "koinon"
	subNS = "ate-system"
)

// pki makes a CA and a leaf for the cache name. The keys are made here and are never real.
func pki(cn string) (caPEM, leafPEM, keyPEM []byte) {
	GinkgoHelper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn + " CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	Expect(err).NotTo(HaveOccurred())
	ca, err := x509.ParseCertificate(caDER)
	Expect(err).NotTo(HaveOccurred())
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"koinon-imagecache.koinon.svc"}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	Expect(err).NotTo(HaveOccurred())
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	Expect(err).NotTo(HaveOccurred())
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func tlsSecret(ca, leaf, key []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "koinon-imagecache-tls", Namespace: relNS}, Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{"ca.crt": ca, "tls.crt": leaf, "tls.key": key},
	}
}

func ateletDS() *appsv1.DaemonSet {
	ds := ateletDSUnpatched()
	ds.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: imagecacheca.VolumeName, VolumeSource: corev1.VolumeSource{
		ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: imagecacheca.ConfigMapName}},
	}}}
	ds.Spec.Template.Spec.Containers = []corev1.Container{{Name: imagecacheca.AteletContainer, Env: []corev1.EnvVar{{Name: imagecacheca.EnvName, Value: imagecacheca.CertDirs}}}}
	return ds
}

// ateletDSUnpatched is atelet as Substrate ships it, before the installer patch.
func ateletDSUnpatched() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "atelet", Namespace: subNS}}
}

func cacheDeploy() *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "koinon-imagecache", Namespace: relNS}}
}

var params = imagecacheca.Params{Secret: "koinon-imagecache-tls", Namespace: relNS, SubstrateNamespace: subNS, Deployment: "koinon-imagecache"}

var _ = Describe("Publish", func() {
	It("copies only the public CA and stamps atelet and the cache", func(ctx SpecContext) {
		ca, leaf, key := pki("cache")
		c := fake.NewClientset(tlsSecret(ca, leaf, key), ateletDS(), cacheDeploy())
		var out bytes.Buffer
		Expect(imagecacheca.Publish(ctx, c, params, &out)).To(Succeed())

		cm, err := c.CoreV1().ConfigMaps(subNS).Get(ctx, imagecacheca.ConfigMapName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(cm.Data).To(HaveLen(1))
		Expect(cm.Data["ca.crt"]).To(Equal(string(ca)))
		Expect(cm.BinaryData).To(BeEmpty())
		Expect(cm.Data["ca.crt"]).NotTo(ContainSubstring("PRIVATE KEY"))
		Expect(out.String()).NotTo(ContainSubstring("PRIVATE KEY"))

		ds, err := c.AppsV1().DaemonSets(subNS).Get(ctx, "atelet", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(ds.Spec.Template.Annotations).To(HaveKeyWithValue(imagecacheca.AteletAnnotation, imagecacheca.Sum(ca)))
		d, err := c.AppsV1().Deployments(relNS).Get(ctx, "koinon-imagecache", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Spec.Template.Annotations).To(HaveKeyWithValue(imagecacheca.CacheAnnotation, imagecacheca.Sum(leaf, ca)))
	})

	It("changes nothing on a second run", func(ctx SpecContext) {
		ca, leaf, key := pki("cache")
		c := fake.NewClientset(tlsSecret(ca, leaf, key), ateletDS(), cacheDeploy())
		Expect(imagecacheca.Publish(ctx, c, params, &bytes.Buffer{})).To(Succeed())
		c.ClearActions()
		var out bytes.Buffer
		Expect(imagecacheca.Publish(ctx, c, params, &out)).To(Succeed())
		for _, a := range c.Actions() {
			Expect(a.GetVerb()).To(BeElementOf("get"), "a second run must only read: %v", a)
		}
		Expect(out.String()).To(ContainSubstring("is current"))
	})

	It("rolls atelet and the cache when the CA rotates", func(ctx SpecContext) {
		ca, leaf, key := pki("cache")
		c := fake.NewClientset(tlsSecret(ca, leaf, key), ateletDS(), cacheDeploy())
		Expect(imagecacheca.Publish(ctx, c, params, &bytes.Buffer{})).To(Succeed())

		ca2, leaf2, key2 := pki("cache")
		_, err := c.CoreV1().Secrets(relNS).Update(ctx, tlsSecret(ca2, leaf2, key2), metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(imagecacheca.Publish(ctx, c, params, &bytes.Buffer{})).To(Succeed())

		cm, _ := c.CoreV1().ConfigMaps(subNS).Get(ctx, imagecacheca.ConfigMapName, metav1.GetOptions{})
		Expect(cm.Data["ca.crt"]).To(Equal(string(ca2)))
		ds, _ := c.AppsV1().DaemonSets(subNS).Get(ctx, "atelet", metav1.GetOptions{})
		Expect(ds.Spec.Template.Annotations[imagecacheca.AteletAnnotation]).To(Equal(imagecacheca.Sum(ca2)))
		d, _ := c.AppsV1().Deployments(relNS).Get(ctx, "koinon-imagecache", metav1.GetOptions{})
		Expect(d.Spec.Template.Annotations[imagecacheca.CacheAnnotation]).To(Equal(imagecacheca.Sum(leaf2, ca2)))
	})

	It("rolls the cache, not atelet, when only the leaf changes", func(ctx SpecContext) {
		ca, leaf, key := pki("cache")
		c := fake.NewClientset(tlsSecret(ca, leaf, key), ateletDS(), cacheDeploy())
		Expect(imagecacheca.Publish(ctx, c, params, &bytes.Buffer{})).To(Succeed())
		_, leaf2, key2 := pki("cache")
		_, err := c.CoreV1().Secrets(relNS).Update(ctx, tlsSecret(ca, leaf2, key2), metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(imagecacheca.Publish(ctx, c, params, &bytes.Buffer{})).To(Succeed())
		ds, _ := c.AppsV1().DaemonSets(subNS).Get(ctx, "atelet", metav1.GetOptions{})
		Expect(ds.Spec.Template.Annotations[imagecacheca.AteletAnnotation]).To(Equal(imagecacheca.Sum(ca)))
		d, _ := c.AppsV1().Deployments(relNS).Get(ctx, "koinon-imagecache", metav1.GetOptions{})
		Expect(d.Spec.Template.Annotations[imagecacheca.CacheAnnotation]).To(Equal(imagecacheca.Sum(leaf2, ca)))
	})

	It("accepts a cache Deployment that does not exist yet (the first install)", func(ctx SpecContext) {
		ca, leaf, key := pki("cache")
		c := fake.NewClientset(tlsSecret(ca, leaf, key), ateletDS())
		var out bytes.Buffer
		Expect(imagecacheca.Publish(ctx, c, params, &out)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("does not exist yet"))
	})

	It("fails, and changes nothing, when atelet has no installer patch", func(ctx SpecContext) {
		ca, leaf, key := pki("cache")
		c := fake.NewClientset(tlsSecret(ca, leaf, key), ateletDSUnpatched(), cacheDeploy())
		err := imagecacheca.Publish(ctx, c, params, io.Discard)
		Expect(err).To(MatchError(ContainSubstring("run koinon-api substrate install")))
		_, err = c.CoreV1().ConfigMaps(subNS).Get(ctx, imagecacheca.ConfigMapName, metav1.GetOptions{})
		Expect(err).To(HaveOccurred())
	})

	It("fails with the way out when atelet is not installed", func(ctx SpecContext) {
		ca, leaf, key := pki("cache")
		c := fake.NewClientset(tlsSecret(ca, leaf, key))
		err := imagecacheca.Publish(ctx, c, params, &bytes.Buffer{})
		Expect(err).To(MatchError(ContainSubstring("koinon-api substrate install")))
	})

	It("refuses a ca.crt that carries a private key", func(ctx SpecContext) {
		ca, leaf, key := pki("cache")
		c := fake.NewClientset(tlsSecret(append(append([]byte{}, ca...), key...), leaf, key), ateletDS())
		err := imagecacheca.Publish(ctx, c, params, &bytes.Buffer{})
		Expect(err).To(MatchError(imagecacheca.ErrCA))
		Expect(err).To(MatchError(ContainSubstring("EC PRIVATE KEY")))
		_, getErr := c.CoreV1().ConfigMaps(subNS).Get(ctx, imagecacheca.ConfigMapName, metav1.GetOptions{})
		Expect(getErr).To(HaveOccurred(), "nothing may be published")
	})

	DescribeTable("refuses a bad ca.crt",
		func(ctx SpecContext, raw string) {
			_, leaf, key := pki("cache")
			c := fake.NewClientset(tlsSecret([]byte(raw), leaf, key), ateletDS())
			Expect(imagecacheca.Publish(ctx, c, params, &bytes.Buffer{})).To(MatchError(imagecacheca.ErrCA))
		},
		Entry("empty", ""),
		Entry("text", "not a certificate"),
		Entry("a certificate block that does not parse", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"),
	)

	It("fails for a missing Secret", func(ctx SpecContext) {
		c := fake.NewClientset(ateletDS())
		Expect(imagecacheca.Publish(ctx, c, params, &bytes.Buffer{})).To(MatchError(ContainSubstring("read Secret koinon/koinon-imagecache-tls")))
	})
})

// Go reads the system roots from the default file and then adds the certificate files of SSL_CERT_DIR. atelet relies on
// that: the installer sets only SSL_CERT_DIR, so the public roots must stay. The check runs a child process because Go
// reads the variables once.
var _ = Describe("SSL_CERT_DIR", func() {
	It("adds the cache CA and keeps the default roots", func() {
		if os.Getenv("KOINON_IMAGECACHECA_CHILD") != "" {
			Skip("child process")
		}
		ca, _, _ := pki("cache")
		dir := GinkgoT().TempDir()
		// What a ConfigMap volume looks like: ca.crt is a symlink into a versioned directory.
		Expect(os.MkdirAll(filepath.Join(dir, "..2026_11_20"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "..2026_11_20", "ca.crt"), ca, 0o600)).To(Succeed())
		Expect(os.Symlink("..2026_11_20", filepath.Join(dir, "..data"))).To(Succeed())
		Expect(os.Symlink(filepath.Join("..data", "ca.crt"), filepath.Join(dir, "ca.crt"))).To(Succeed())

		exe, err := os.Executable()
		Expect(err).NotTo(HaveOccurred())
		run := func(env ...string) string {
			cmd := exec.Command(exe, "-test.run=^TestRootsChild$")
			cmd.Env = append(os.Environ(), append(env, "KOINON_IMAGECACHECA_CHILD="+filepath.Join(dir, "ca.crt"))...)
			out, err := cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), string(out))
			return string(out)
		}
		Expect(run("SSL_CERT_DIR=" + dir)).To(ContainSubstring("CA trusted=true"))
		Expect(run("SSL_CERT_DIR=" + dir)).To(MatchRegexp(`default roots kept=(true|n/a)`))
		Expect(run()).To(ContainSubstring("CA trusted=false"))
	})
})
