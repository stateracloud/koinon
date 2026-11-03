// SPDX-License-Identifier: Apache-2.0

package awskms_test

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/awskms"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
)

// static hands out fakeCreds, counting the fetches.
func static(n *atomic.Int64) func(context.Context) (awskms.Credentials, error) {
	return func(context.Context) (awskms.Credentials, error) {
		if n != nil {
			n.Add(1)
		}
		return fakeCreds, nil
	}
}

// withKey is the adapter over LocalStack's key.
func withKey(key string) *awskms.Wrapper {
	GinkgoHelper()
	w, err := awskms.NewWithCredentials(awskms.Config{Region: region, KeyID: key, Endpoint: localstack}, static(nil))
	Expect(err).NotTo(HaveOccurred())
	return w
}

// hanging is a listener that accepts connections and never answers.
func hanging() string {
	GinkgoHelper()
	l, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(l.Close)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			DeferCleanup(c.Close)
		}
	}()
	return "http://" + l.Addr().String()
}

var _ = Describe("the AWS KMS wrapper", func() {
	kek := bytes.Repeat([]byte("k"), secretstore.KeySize) // obviously fake
	aad := []byte("koinon-test-aad")

	Describe("the wrapper contract", func() {
		secretstoretest.WrapperContract(
			func() secretstore.Wrapper { return withKey(testKey) },
			func() secretstore.Wrapper {
				w, err := awskms.NewWithCredentials(awskms.Config{Region: region, KeyID: testKey, Endpoint: hanging()}, static(nil))
				Expect(err).NotTo(HaveOccurred())
				return w
			},
		)
	})

	It("names its backend and the key as configured", func() {
		w := withKey(testKey)
		Expect(w.Backend()).To(Equal("awskms"))
		Expect(w.KeyID()).To(Equal(testKey))
	})

	It("unwraps through an alias what it wrapped through the key ARN", func(ctx SpecContext) {
		wrapped, err := withKey(testKey).Wrap(ctx, kek, aad)
		Expect(err).NotTo(HaveOccurred())
		got, err := withKey("alias/koinon-test").Unwrap(ctx, wrapped, aad)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(kek))
		Expect(withKey("alias/koinon-test").Probe(ctx)).To(Succeed())
	})

	It("refuses a ciphertext of another key as not opening, since every decrypt names the key", func(ctx SpecContext) {
		wrapped, err := withKey(otherKey).Wrap(ctx, kek, aad)
		Expect(err).NotTo(HaveOccurred())
		_, err = withKey(testKey).Unwrap(ctx, wrapped, aad)
		Expect(err).To(MatchError(secretstore.ErrOpen))
	})

	It("reports a missing key, a disabled key, a key pending deletion and a stopped KMS as unreachable", func(ctx SpecContext) {
		for _, missing := range []string{"11111111-2222-3333-4444-555555555555", "alias/koinon-test-missing"} {
			w := withKey(missing)
			Expect(w.Probe(ctx)).To(MatchError(secretstore.ErrUnreachable), missing)
			_, err := w.Wrap(ctx, kek, aad)
			Expect(err).To(MatchError(secretstore.ErrUnreachable), missing)
			Expect(err.Error()).To(ContainSubstring("NotFoundException"))
		}

		disabled := createKey(ctx)["KeyId"].(string)
		wrapped, err := withKey(disabled).Wrap(ctx, kek, aad)
		Expect(err).NotTo(HaveOccurred())
		kms(ctx, "DisableKey", map[string]any{"KeyId": disabled})
		err = withKey(disabled).Probe(ctx)
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(err.Error()).To(ContainSubstring("DisabledException"))
		_, err = withKey(disabled).Unwrap(ctx, wrapped, aad)
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(err).NotTo(MatchError(secretstore.ErrOpen))
		kms(ctx, "EnableKey", map[string]any{"KeyId": disabled})
		Expect(withKey(disabled).Probe(ctx)).To(Succeed())

		deleted := createKey(ctx)["KeyId"].(string)
		kms(ctx, "ScheduleKeyDeletion", map[string]any{"KeyId": deleted, "PendingWindowInDays": 7})
		err = withKey(deleted).Probe(ctx)
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(err.Error()).To(ContainSubstring("KMSInvalidStateException"))

		l, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		closed := "http://" + l.Addr().String()
		Expect(l.Close()).To(Succeed())
		down, err := awskms.NewWithCredentials(awskms.Config{Region: region, KeyID: testKey, Endpoint: closed}, static(nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(down.Probe(ctx)).To(MatchError(secretstore.ErrUnreachable))
	})

	It("refuses a key that is not for encryption as unreachable, not healthy", func(ctx SpecContext) {
		mac := kms(ctx, "CreateKey", map[string]any{"KeySpec": "HMAC_256", "KeyUsage": "GENERATE_VERIFY_MAC"})["KeyMetadata"].(map[string]any)
		err := withKey(mac["KeyId"].(string)).Probe(ctx)
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(err.Error()).To(ContainSubstring("InvalidKeyUsageException"))
	})

	It("binds a KEK to its ID through secretstore.Wrappers", func(ctx SpecContext) {
		ws, err := secretstore.NewKeyAdminWrappers(withKey(testKey))
		Expect(err).NotTo(HaveOccurred())
		stored, err := ws.Wrap(ctx, "koinon-test-1", kek)
		Expect(err).NotTo(HaveOccurred())
		Expect(stored.Backend).To(Equal("awskms"))
		Expect(stored.KeyID).To(Equal(testKey))
		got, err := ws.Unwrap(ctx, "koinon-test-1", stored)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(kek))
		_, err = ws.Unwrap(ctx, "koinon-test-2", stored)
		Expect(err).To(MatchError(secretstore.ErrOpen))

		svc, err := secretstore.NewWrappers(withKey(testKey))
		Expect(err).NotTo(HaveOccurred())
		got, err = svc.Unwrap(ctx, "koinon-test-1", stored)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(kek))
		_, err = svc.Wrap(ctx, "koinon-test-3", kek)
		Expect(err).To(MatchError(secretstore.ErrKeyAdminWraps))
	})

	It("signs in with IRSA through STS AssumeRoleWithWebIdentity", func(ctx SpecContext) {
		p := filepath.Join(GinkgoT().TempDir(), "token")
		Expect(os.WriteFile(p, []byte("koinon_test_not-a-real-jwt"), 0o600)).To(Succeed())
		w, err := awskms.NewAnyHTTP(awskms.Config{
			Region: region, KeyID: testKey, Endpoint: localstack, STSEndpoint: localstack,
			Identity: awskms.Identity{RoleARN: "arn:aws:iam::000000000000:role/koinon-api", WebIdentityTokenFile: p},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Probe(ctx)).To(Succeed())
		wrapped, err := w.Wrap(ctx, kek, aad)
		Expect(err).NotTo(HaveOccurred())
		got, err := w.Unwrap(ctx, wrapped, aad)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(kek))
	})
})
