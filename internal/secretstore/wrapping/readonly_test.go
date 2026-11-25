// SPDX-License-Identifier: Apache-2.0

package wrapping_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/secretstore/wrapping"
)

// testInstall is the install ID the KEKs are bound to (old issue 615); obviously fake.
const testInstall = "00000000-0000-4000-8000-000000000688"

// fakeKey is a FakeKMS that answers as the backend of the key it stands in for, so a KEK it wraps
// is stored as that backend's and only that backend's adapter opens it.
type fakeKey struct {
	*secretstoretest.FakeKMS
	backend string
}

func (f fakeKey) Backend() string { return f.backend }

// fakeKMS stands in for the OpenBao and AWS KMS servers: one fake per backend and key, kept across
// New calls, so a spec can count the wraps each key got.
type fakeKMS map[string]*secretstoretest.FakeKMS

func (fs fakeKMS) build(k wrapping.Key) (secretstore.Wrapper, error) {
	// The key IDs are the real adapters' (transit mount and key; the AWS key as spelled).
	id, name := k.OpenBao.TransitMount+"/"+k.OpenBao.Key, k.Backend+":"+k.OpenBao.Key
	if k.Backend == wrapping.BackendAWSKMS {
		id, name = k.AWSKMS.KeyID, k.Backend+":"+k.AWSKMS.KeyID
	}
	if fs[name] == nil {
		fs[name] = &secretstoretest.FakeKMS{Name: id}
	}
	return fakeKey{FakeKMS: fs[name], backend: k.Backend}, nil
}

func (fs fakeKMS) wraps() int64 {
	var n int64
	for _, f := range fs {
		n += f.Wraps()
	}
	return n
}

var _ = Describe("the read-only wrapping key (issue 688)", func() {
	// kek is an obviously fake KEK; its text is what a leak would show in a log.
	kek := []byte("koinon_test_not-a-real-key-00688")
	const id = "k-old"

	var (
		kms  fakeKMS
		logs bytes.Buffer
	)
	BeforeEach(func() {
		Expect(kek).To(HaveLen(secretstore.KeySize))
		kms = fakeKMS{}
		DeferCleanup(wrapping.SetAdapter(kms.build))
		logs.Reset()
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		DeferCleanup(func() { slog.SetDefault(prev) })
	})

	oldBao := []string{"--openbao-address=https://bao.example:8200", "--openbao-key=koinon-kek-old", "--openbao-role=koinon-api"}
	readOnlyBao := []string{
		"--read-only-kek-backend=openbao", "--read-only-openbao-address=https://bao.example:8200",
		"--read-only-openbao-key=koinon-kek-old", "--read-only-openbao-role=koinon-api",
	}
	newAWS := []string{"--kek-backend=awskms", "--awskms-region=eu-central-1", "--awskms-key-id=alias/koinon-kek-new"}

	build := func(ctx context.Context, role wrapping.Role, args ...string) secretstore.Wrappers {
		GinkgoHelper()
		ws, err := wrapping.New(ctx, parse(args...), role, unset)
		Expect(err).NotTo(HaveOccurred())
		return ws.WithInstallID(secretstore.StaticInstallID(testInstall))
	}

	// stored is the KEK as the key-admin Job wrapped it with the old OpenBao key, before the move.
	stored := func(ctx context.Context) secretstore.StoredKEK {
		GinkgoHelper()
		s, err := build(ctx, wrapping.KeyAdmin, append([]string{"--kek-backend=openbao"}, oldBao...)...).Wrap(ctx, id, kek)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Backend).To(Equal(wrapping.BackendOpenBao))
		Expect(s.KeyID).To(Equal("transit/koinon-kek-old"))
		return s
	}

	// load loads a KEK Secret that holds s as the active KEK, as koinon-api does on start.
	load := func(ctx context.Context, ws secretstore.Wrappers, s secretstore.StoredKEK) (*secretstore.Keyring, error) {
		GinkgoHelper()
		dir := GinkgoT().TempDir()
		b, err := s.Encode()
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(dir, id), b, 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, secretstore.ActiveFile), []byte(id), 0o600)).To(Succeed())
		return secretstore.LoadDir(ctx, dir, ws)
	}

	It("loads a KEK of the old key after a move to a new KMS key, and after a move back to local, in every role", func(ctx SpecContext) {
		s := stored(ctx)
		for _, role := range []wrapping.Role{wrapping.Service, wrapping.KeyAdmin} {
			for name, args := range map[string][]string{
				"a new AWS KMS key": append(newAWS, readOnlyBao...),
				"local":             readOnlyBao,
			} {
				By("a move to " + name)
				ws := build(ctx, role, args...)
				ring, err := load(ctx, ws, s)
				Expect(err).NotTo(HaveOccurred(), name)
				Expect(ring.Active()).To(Equal(id))
				// The old KEK is routed to the old key's adapter, and that one is read-only.
				w, err := ws.For(s)
				Expect(err).NotTo(HaveOccurred())
				Expect(w.Backend()).To(Equal(wrapping.BackendOpenBao))
				Expect(w.KeyID()).To(Equal("transit/koinon-kek-old"))
				Expect(ws.Primary().KeyID()).NotTo(Equal(w.KeyID()))
				_, err = w.Wrap(ctx, kek, nil)
				Expect(err).To(MatchError(wrapping.ErrReadOnlyKey))
				got, err := ws.Unwrap(ctx, id, s)
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal(kek))
				Expect(ws.Probe(ctx, s)).To(Succeed(), "the old key is probed with decrypt rights only")
			}
		}
	})

	It("never wraps with the read-only key: a new KEK goes to the primary, and the key itself refuses", func(ctx SpecContext) {
		s := stored(ctx)
		old := kms["openbao:koinon-kek-old"]
		before, oldBefore := kms.wraps(), old.Wraps()

		// After a move to a new KMS key, the key-admin Job wraps with the new key only.
		admin := build(ctx, wrapping.KeyAdmin, append(newAWS, readOnlyBao...)...)
		n, err := admin.Wrap(ctx, "k-new", kek)
		Expect(err).NotTo(HaveOccurred())
		Expect(n.Backend).To(Equal(wrapping.BackendAWSKMS))
		Expect(n.KeyID).To(Equal("alias/koinon-kek-new"))
		Expect(old.Wraps()).To(Equal(oldBefore), "the old key got no wrap after the move")

		// After a move to local, a new KEK is local, and a service never calls a KMS to wrap.
		svc := build(ctx, wrapping.Service, readOnlyBao...)
		l, err := svc.Wrap(ctx, "k-local", kek)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Backend).To(Equal(secretstore.BackendLocal))
		Expect(old.Wraps()).To(Equal(oldBefore))

		// The adapter of the old key refuses a wrap before it calls the KMS, even if asked directly.
		for _, ws := range []secretstore.Wrappers{admin, svc} {
			w, err := ws.For(s)
			Expect(err).NotTo(HaveOccurred())
			_, err = w.Wrap(ctx, kek, []byte("koinon-test-aad"))
			Expect(err).To(MatchError(wrapping.ErrReadOnlyKey))
		}
		Expect(old.Wraps()).To(Equal(oldBefore))
		Expect(kms.wraps()-before).To(Equal(int64(1)), "only the one wrap with the new AWS KMS key")
	})

	It("fails closed when neither key opens a KEK", func(ctx SpecContext) {
		s := stored(ctx)

		By("a move without the read-only key: no adapter for the old key")
		ws := build(ctx, wrapping.Service, newAWS...)
		_, err := load(ctx, ws, s)
		Expect(err).To(MatchError(ContainSubstring("no adapter is configured")))
		_, err = ws.Unwrap(ctx, id, s)
		Expect(err).To(HaveOccurred())
		Expect(ws.Probe(ctx, s)).To(MatchError(secretstore.ErrUnreachable))

		By("a read-only key that is another key than the one that wrapped the KEK")
		other := []string{"--read-only-kek-backend=awskms", "--read-only-awskms-region=eu-central-1", "--read-only-awskms-key-id=alias/koinon-kek-other"}
		ws = build(ctx, wrapping.Service, other...)
		_, err = load(ctx, ws, s)
		Expect(err).To(MatchError(ContainSubstring("no adapter is configured")))

		By("the right read-only key, and a KEK that does not open under it")
		ws = build(ctx, wrapping.Service, append(newAWS, readOnlyBao...)...)
		bad := s
		bad.Wrapped = bytes.Clone(s.Wrapped)
		bad.Wrapped[len(bad.Wrapped)-1] ^= 1
		_, err = load(ctx, ws, bad)
		Expect(err).To(MatchError(secretstore.ErrOpen))

		By("the right read-only key, and the KEK of another install")
		_, err = ws.WithInstallID(secretstore.StaticInstallID("00000000-0000-4000-8000-000000000999")).Unwrap(ctx, id, s)
		Expect(err).To(MatchError(secretstore.ErrOpen))

		By("the old key down: the load fails, it does not go on without the KEK")
		kms["openbao:koinon-kek-old"].Down.Store(true)
		_, err = load(ctx, ws, s)
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
	})

	It("refuses a read-only key that is the primary, a local one, an unknown backend or an incomplete one", func(ctx SpecContext) {
		_, err := wrapping.New(ctx, parse(append([]string{"--kek-backend=openbao"}, append(oldBao, readOnlyBao...)...)...), wrapping.Service, unset)
		Expect(err).To(MatchError(ContainSubstring("configured twice")))
		_, err = wrapping.New(ctx, parse("--read-only-kek-backend=local"), wrapping.Service, unset)
		Expect(err).To(MatchError(ContainSubstring("local KEKs always unwrap")))
		_, err = wrapping.New(ctx, parse("--read-only-kek-backend=vaultish"), wrapping.KeyAdmin, unset)
		Expect(err).To(MatchError(ContainSubstring("unknown read-only KEK backend")))
	})

	It("writes no key material to the logs, on a load, a refused wrap and a failed load", func(ctx SpecContext) {
		s := stored(ctx)
		ws := build(ctx, wrapping.KeyAdmin, append(newAWS, readOnlyBao...)...)
		_, err := load(ctx, ws, s)
		Expect(err).NotTo(HaveOccurred())
		w, err := ws.For(s)
		Expect(err).NotTo(HaveOccurred())
		_, err = w.Wrap(ctx, kek, nil)
		Expect(err).To(HaveOccurred())
		// The error with the most context: the KEK of another install.
		_, installErr := ws.WithInstallID(secretstore.StaticInstallID("00000000-0000-4000-8000-000000000999")).Unwrap(ctx, id, s)
		Expect(installErr).To(MatchError(secretstore.ErrOpen))
		kms["openbao:koinon-kek-old"].Down.Store(true)
		_, loadErr := load(ctx, ws, s)
		Expect(loadErr).To(HaveOccurred())

		// The read-only settings without a read-only backend are named in a warning, never valued.
		_, err = wrapping.New(ctx, parse("--read-only-openbao-key=koinon-kek-old"), wrapping.Service, unset)
		Expect(err).NotTo(HaveOccurred())
		Expect(logs.String()).To(ContainSubstring("the read-only key's settings are ignored"))
		Expect(logs.String()).To(ContainSubstring("read-only-openbao-key"))
		Expect(logs.String()).NotTo(ContainSubstring("koinon-kek-old"))

		for _, leak := range []string{
			string(kek), base64.StdEncoding.EncodeToString(kek), hex.EncodeToString(kek),
			base64.StdEncoding.EncodeToString(s.Wrapped), "not-a-real-key",
		} {
			Expect(logs.String()).NotTo(ContainSubstring(leak))
			Expect(loadErr.Error()).NotTo(ContainSubstring(leak))
			Expect(installErr.Error()).NotTo(ContainSubstring(leak))
		}
	})

	It("builds the read-only key from its flags with the real adapters, without calling them", func(ctx SpecContext) {
		DeferCleanup(wrapping.SetAdapter(wrapping.RealAdapter))
		GinkgoT().Setenv("AWS_ROLE_ARN", "arn:aws:iam::111122223333:role/koinon-api")
		GinkgoT().Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "/var/run/secrets/eks.amazonaws.com/serviceaccount/token")
		readOnlyAWS := []string{"--read-only-kek-backend=awskms", "--read-only-awskms-region=eu-central-1", "--read-only-awskms-key-id=alias/koinon-kek-old"}
		for _, c := range []struct {
			args           []string
			backend, keyID string
		}{
			{append(newAWS, readOnlyBao...), wrapping.BackendOpenBao, "transit/koinon-kek-old"},
			{readOnlyBao, wrapping.BackendOpenBao, "transit/koinon-kek-old"},
			{append(append([]string{"--kek-backend=openbao"}, oldBao...), readOnlyAWS...), wrapping.BackendAWSKMS, "alias/koinon-kek-old"},
		} {
			for _, role := range []wrapping.Role{wrapping.Service, wrapping.KeyAdmin} {
				ws, err := wrapping.New(ctx, parse(c.args...), role, unset)
				Expect(err).NotTo(HaveOccurred())
				w, err := ws.For(secretstore.StoredKEK{Backend: c.backend, KeyID: c.keyID})
				Expect(err).NotTo(HaveOccurred())
				Expect(w.Backend()).To(Equal(c.backend))
				Expect(w.KeyID()).To(Equal(c.keyID))
				Expect(ws.Primary().KeyID()).NotTo(Equal(c.keyID), "the read-only key is never the primary")
				// The refusal comes before any call: there is no server at these addresses.
				_, err = w.Wrap(ctx, kek, nil)
				Expect(err).To(MatchError(wrapping.ErrReadOnlyKey))
			}
		}
		_, err := wrapping.New(ctx, parse("--read-only-kek-backend=openbao", "--read-only-openbao-address=https://bao.example:8200"), wrapping.Service, unset)
		Expect(err).To(MatchError(ContainSubstring("the read-only wrapping key")), "an incomplete read-only key is refused")
	})
})
