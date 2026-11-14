// SPDX-License-Identifier: Apache-2.0

package secretstore_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
)

// Golden files: one byte-exact vector for each stored format of this package, in testdata/.
// They are what releases wrote to Secrets and to Postgres, so a change that makes a spec here fail
// is a break of stored data, not a test to update.
//
// The six stored formats:
//
//	envelope_v1.json      a sealed value (Envelope, fmt 1), the JSON in an encrypted column
//	kek_local.base64      a local KEK in the KEK Secret: the base64 of 32 bytes
//	kek_local.hex         a local KEK as 64 hex characters (read, never written)
//	kek_wrapped_v1.json   a KMS-wrapped KEK, format 1: bound to its KEK ID
//	kek_wrapped_v2.json   a KMS-wrapped KEK, format 2: bound to its KEK ID and the install
//	active                the file that names the active KEK
//
// Never edit a file by hand. To write them again (only for a deliberate, migrated format change,
// or when a file is added):
//
//	KOINON_UPDATE_GOLDEN=1 go test ./internal/secretstore -run TestSecretstore
//
// Sealing and wrapping use random nonces, so the files change on every regeneration; the secrets
// in them are fixed, obviously fake values.
const updateGoldenEnv = "KOINON_UPDATE_GOLDEN"

const (
	goldenPlaintext = "koinon_test_not-a-real-token"
	goldenKEKID     = "golden-kek"
	goldenInstall   = "00000000-0000-4000-8000-000000000685"
)

var (
	// goldenKEK is 32 times the letter k: obviously fake and low-entropy.
	goldenKEK     = bytes.Repeat([]byte("k"), secretstore.KeySize)
	goldenBinding = secretstore.Binding{OrgID: "org-golden", Table: "providers", RowID: "row-golden", Column: "api_key"}
)

// goldenFiles is every golden file name, for the check that testdata/ holds no other.
var goldenFiles = []string{"envelope_v1.json", "kek_local.base64", "kek_local.hex", "kek_wrapped_v1.json", "kek_wrapped_v2.json", "active"}

// lp is the length-prefixed encoding of the associated data, written again here on purpose: the
// spec pins the bytes the code authenticates, so a change of its encoding cannot go unnoticed.
func lp(fields ...string) []byte {
	var out []byte
	for _, f := range fields {
		out = binary.BigEndian.AppendUint32(out, uint32(len(f)))
		out = append(out, f...)
	}
	return out
}

func goldenPath(name string) string { return filepath.Join("testdata", name) }

func readGolden(name string) []byte {
	GinkgoHelper()
	b, err := os.ReadFile(goldenPath(name))
	Expect(err).NotTo(HaveOccurred(), "missing %s; see the note in golden_test.go", name)
	return b
}

// writeGolden regenerates the golden files with the code at head.
func writeGolden(ctx context.Context) {
	GinkgoHelper()
	Expect(os.MkdirAll("testdata", 0o755)).To(Succeed())
	put := func(name string, b []byte) {
		Expect(os.WriteFile(goldenPath(name), b, 0o644)).To(Succeed())
	}

	ring, err := secretstore.NewKeyring(map[string][]byte{goldenKEKID: goldenKEK}, goldenKEKID)
	Expect(err).NotTo(HaveOccurred())
	env, err := ring.Seal(goldenBinding, []byte(goldenPlaintext))
	Expect(err).NotTo(HaveOccurred())
	raw, err := json.Marshal(env)
	Expect(err).NotTo(HaveOccurred())
	put("envelope_v1.json", append(raw, '\n'))

	put("kek_local.base64", append(secretstore.EncodeKey(goldenKEK), '\n'))
	put("kek_local.hex", []byte(hex.EncodeToString(goldenKEK)+"\n"))

	kms := &secretstoretest.FakeKMS{}
	wrapped1, err := kms.Wrap(ctx, goldenKEK, lp("koinon/secretstore/v1", "kek", goldenKEKID, "", "", "", ""))
	Expect(err).NotTo(HaveOccurred())
	v1, err := secretstore.StoredKEK{Backend: kms.Backend(), KeyID: kms.KeyID(), Wrapped: wrapped1}.Encode()
	Expect(err).NotTo(HaveOccurred())
	put("kek_wrapped_v1.json", append(v1, '\n'))

	wrapped2, err := kms.Wrap(ctx, goldenKEK, lp("koinon/secretstore/v1", "kek-install", goldenKEKID, goldenInstall))
	Expect(err).NotTo(HaveOccurred())
	v2, err := secretstore.StoredKEK{Backend: kms.Backend(), KeyID: kms.KeyID(), Wrapped: wrapped2, InstallID: goldenInstall}.Encode()
	Expect(err).NotTo(HaveOccurred())
	put("kek_wrapped_v2.json", append(v2, '\n'))

	put("active", []byte(goldenKEKID+"\n"))
}

var _ = Describe("stored formats (golden files in testdata/)", func() {
	BeforeEach(func(ctx SpecContext) {
		if os.Getenv(updateGoldenEnv) != "" {
			writeGolden(ctx)
		}
	})

	It("holds exactly the six golden files", func() {
		ents, err := os.ReadDir("testdata")
		Expect(err).NotTo(HaveOccurred())
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		Expect(names).To(ConsistOf(goldenFiles))
	})

	It("opens the sealed value (envelope, fmt 1) and keeps its field names", func() {
		raw := readGolden("envelope_v1.json")
		var fields map[string]any
		Expect(json.Unmarshal(raw, &fields)).To(Succeed())
		Expect(fields).To(HaveLen(7))
		for _, k := range []string{"fmt", "alg", "kek_id", "wrapped_dek", "dek_nonce", "nonce", "ciphertext"} {
			Expect(fields).To(HaveKey(k))
		}
		Expect(fields).To(HaveKeyWithValue("fmt", BeNumerically("==", 1)))
		Expect(fields).To(HaveKeyWithValue("alg", "AES-256-GCM"))
		Expect(fields).To(HaveKeyWithValue("kek_id", goldenKEKID))

		var env secretstore.Envelope
		Expect(json.Unmarshal(raw, &env)).To(Succeed())
		ring, err := secretstore.NewKeyring(map[string][]byte{goldenKEKID: goldenKEK}, goldenKEKID)
		Expect(err).NotTo(HaveOccurred())
		got, err := ring.Open(goldenBinding, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal(goldenPlaintext))

		By("authenticating its binding")
		other := goldenBinding
		other.RowID = "row-other"
		_, err = ring.Open(other, env)
		Expect(err).To(MatchError(secretstore.ErrOpen))
	})

	It("reads a local KEK as base64 and as hex", func() {
		Expect(string(readGolden("kek_local.base64"))).To(Equal(base64.StdEncoding.EncodeToString(goldenKEK) + "\n"))
		for _, name := range []string{"kek_local.base64", "kek_local.hex"} {
			keys, _, err := secretstore.ParseStoredKeys(map[string][]byte{goldenKEKID: readGolden(name)})
			Expect(err).NotTo(HaveOccurred(), name)
			Expect(keys).To(HaveLen(1))
			s := keys[goldenKEKID]
			Expect(s.Backend).To(Equal(secretstore.BackendLocal), name)
			Expect(s.Wrapped).To(Equal(goldenKEK), name)
			Expect(s.Bound()).To(BeFalse(), name)
		}
		By("writing the base64 form as before")
		enc, err := secretstore.StoredKEK{Backend: secretstore.BackendLocal, Wrapped: goldenKEK}.Encode()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(enc) + "\n").To(Equal(string(readGolden("kek_local.base64"))))
	})

	It("reads the wrapped KEK of format 1, unwraps it with the ID-bound data and writes it back unchanged", func(ctx SpecContext) {
		raw := readGolden("kek_wrapped_v1.json")
		var fields map[string]any
		Expect(json.Unmarshal(raw, &fields)).To(Succeed())
		Expect(fields).To(HaveKeyWithValue("fmt", BeNumerically("==", 1)))
		Expect(fields).To(HaveKeyWithValue("backend", secretstoretest.FakeKMSBackend))
		Expect(fields).NotTo(HaveKey("install"))

		keys, _, err := secretstore.ParseStoredKeys(map[string][]byte{goldenKEKID: raw})
		Expect(err).NotTo(HaveOccurred())
		s := keys[goldenKEKID]
		Expect(s.Bound()).To(BeFalse())
		Expect(s.KeyID).To(Equal("koinon-test"))

		ws, err := secretstore.NewWrappers(secretstore.StaticBoundOnly(secretstore.BoundOnly{}), &secretstoretest.FakeKMS{})
		Expect(err).NotTo(HaveOccurred())
		kek, err := ws.Unwrap(ctx, goldenKEKID, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(kek).To(Equal(goldenKEK))

		again, err := s.Encode()
		Expect(err).NotTo(HaveOccurred())
		Expect(bytes.TrimSpace(raw)).To(Equal(again))

		By("refusing it under another KEK ID")
		_, err = ws.Unwrap(ctx, "golden-other", s)
		Expect(err).To(MatchError(secretstore.ErrOpen))
	})

	It("reads the wrapped KEK of format 2, unwraps it with the install-bound data and writes it back unchanged", func(ctx SpecContext) {
		raw := readGolden("kek_wrapped_v2.json")
		var fields map[string]any
		Expect(json.Unmarshal(raw, &fields)).To(Succeed())
		Expect(fields).To(HaveKeyWithValue("fmt", BeNumerically("==", 2)))
		Expect(fields).To(HaveKeyWithValue("install", goldenInstall))

		keys, _, err := secretstore.ParseStoredKeys(map[string][]byte{goldenKEKID: raw})
		Expect(err).NotTo(HaveOccurred())
		s := keys[goldenKEKID]
		Expect(s.Bound()).To(BeTrue())
		Expect(s.InstallID).To(Equal(goldenInstall))

		ws, err := secretstore.NewWrappers(secretstore.StaticBoundOnly(secretstore.BoundOnly{}), &secretstoretest.FakeKMS{})
		Expect(err).NotTo(HaveOccurred())
		kek, err := ws.WithInstallID(secretstore.StaticInstallID(goldenInstall)).Unwrap(ctx, goldenKEKID, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(kek).To(Equal(goldenKEK))

		again, err := s.Encode()
		Expect(err).NotTo(HaveOccurred())
		Expect(bytes.TrimSpace(raw)).To(Equal(again))

		By("refusing it for another install")
		_, err = ws.WithInstallID(secretstore.StaticInstallID("00000000-0000-4000-8000-000000000001")).Unwrap(ctx, goldenKEKID, s)
		Expect(err).To(MatchError(secretstore.ErrOpen))
	})

	It("reads the key set: the active file names the KEK, local and wrapped KEKs side by side", func(ctx SpecContext) {
		data := map[string][]byte{
			"local-b64": readGolden("kek_local.base64"),
			"local-hex": readGolden("kek_local.hex"),
			goldenKEKID: readGolden("kek_wrapped_v2.json"),
			"active":    readGolden("active"),
			"..data":    []byte("the kubelet's directory, skipped"),
		}
		stored, active, err := secretstore.ParseKeySet(data)
		Expect(err).NotTo(HaveOccurred())
		Expect(active).To(Equal(goldenKEKID))
		Expect(stored).To(HaveLen(3))

		ws, err := secretstore.NewWrappers(secretstore.StaticBoundOnly(secretstore.BoundOnly{}), &secretstoretest.FakeKMS{})
		Expect(err).NotTo(HaveOccurred())
		keys, err := ws.WithInstallID(secretstore.StaticInstallID(goldenInstall)).UnwrapAll(ctx, stored)
		Expect(err).NotTo(HaveOccurred())
		for id := range stored {
			Expect(keys[id]).To(Equal(goldenKEK), id)
		}

		By("refusing a format this build does not know")
		_, _, err = secretstore.ParseStoredKeys(map[string][]byte{"k": []byte(`{"fmt":3,"backend":"fakekms","key":"x","wrapped":"AAAA"}`)})
		Expect(err).To(HaveOccurred())
	})
})
