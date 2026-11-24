// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"encoding/json"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/forgecredfile"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/egress"
	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/pricing"
	"github.com/stateracloud/koinon/templates"
)

// staticRecipient is the gateway's public key, as gatewaykeys.PublicKeys gives it.
type staticRecipient struct {
	pub secretstore.RecipientPublicKey
}

func (s staticRecipient) Current() (secretstore.RecipientPublicKey, error) { return s.pub, nil }

var _ = Describe("The forge credential of an agent (#1086 part C)", func() {
	const mcpSecret, mcpDir = "koinon-gateway-mcp", "/secrets/mcp"
	var (
		kube *fake.Clientset
		mg   *fakeModelGateway
		gk   *secretstore.RecipientKey
	)

	BeforeEach(func() {
		kube = fake.NewClientset()
		mg = &fakeModelGateway{}
		var err error
		gk, err = secretstore.GenerateRecipientKey(secretstore.DefaultRecipientSuite())
		Expect(err).NotTo(HaveOccurred())
	})

	// newReconciler builds a reconciler with the gateway key source, or without one when recipient is nil.
	newReconciler := func(recipient reconciler.RecipientKeys) *reconciler.Reconciler {
		GinkgoHelper()
		osg := fakegateway.New()
		cc, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = cc.Close(); osg.Stop() })
		r, err := reconciler.New(pool, openshell.New(cc), mg, reconciler.KubeSecrets{Client: kube, Namespace: namespace}, reconciler.Config{
			KeepWarmPerOrg: 2,
			Prices:         prices.NewResolver(pricing.Embedded()),
			Gateway:        reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Keys:           ring,
			ProviderKeys:   reconciler.KubeProviderKeys{Client: kube, Namespace: namespace, Name: "koinon-gateway-providers"},
			ProviderKeyDir: "/secrets/providers",
			MCPKeys:        reconciler.KubeProviderKeys{Client: kube, Namespace: namespace, Name: mcpSecret},
			MCPKeyDir:      mcpDir, MCPSealed: true, ForgeRecipient: recipient,
			Harnesses: map[string]reconciler.Harness{
				harness.OpenCode: {Image: image, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
			},
			GitHub: fixedTokens{}, URLPolicy: testPolicy,
			Logger: slog.New(slog.NewTextHandler(&syncBuffer{}, nil)),
		})
		Expect(err).NotTo(HaveOccurred())
		return r
	}

	// readAgent makes an active agent of a new org with a repo.read grant on github.com.
	readAgent := func(ctx SpecContext) (org, agent pgtype.UUID) {
		GinkgoHelper()
		org, user := newOrg(ctx)
		id := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		envelope, err := connections.SealFields(ring, org, id, map[string][]byte{connections.GitHubPrivateKeyField: []byte("unused")})
		Expect(err).NotTo(HaveOccurred())
		config, err := json.Marshal(map[string]any{"appId": 1, "installationId": 2})
		Expect(err).NotTo(HaveOccurred())
		conn, err := q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: org, ID: id, Connector: "github", DisplayName: "github", Config: config,
			SecretEnvelope: envelope, Source: store.ConfigSourceUi, CreatedBy: user,
		})
		Expect(err).NotTo(HaveOccurred())
		raw, err := agentconfig.Config{
			Instructions: "help",
			Grants:       []templates.Grant{{Connector: "github", Scopes: []string{"repo.read"}, Preset: templates.PresetStandard}},
			Bindings:     []agentconfig.Binding{{Connector: "github", ConnectionID: uuidOf(conn.ID), Targets: []string{"acme/widgets"}}},
		}.Marshal()
		Expect(err).NotTo(HaveOccurred())
		a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: "Forge helper", Harness: harness.OpenCode, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: a.ID, Config: raw, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		a, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: a.ID, VersionID: version.ID, ActivatedBy: user})
		Expect(err).NotTo(HaveOccurred())
		keepWarm(ctx, org, a.ID)
		return org, a.ID
	}

	credOf := func(agent pgtype.UUID) []route.Credential {
		GinkgoHelper()
		var out []route.Credential
		for _, c := range mg.last().EgressCredentials {
			if c.Agent == execution.UUIDString(agent.Bytes) {
				out = append(out, c)
			}
		}
		return out
	}
	secretData := func(ctx SpecContext) map[string][]byte {
		GinkgoHelper()
		s, err := kube.CoreV1().Secrets(namespace).Get(ctx, mcpSecret, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return s.Data
	}

	It("seals the token to the gateway key, names the file in the state, and the gateway opens it", func(ctx SpecContext) {
		pub := gk.Public()
		r := newReconciler(staticRecipient{pub: pub})
		org, agent := readAgent(ctx)
		Expect(r.ReconcileAll(ctx)).To(Succeed())

		creds := credOf(agent)
		Expect(creds).To(HaveLen(1))
		c := creds[0]
		ref := execution.UUIDString(agent.Bytes)
		Expect(c.Name).To(Equal(reconciler.CodeHostCredentialName(gateway.AgentRef{Org: c.Org, Name: ref})))
		Expect(c.Scheme).To(Equal(route.SchemeBasic))
		Expect(c.User).To(Equal(reconciler.ForgeUser))
		Expect(path.Dir(c.File)).To(Equal(mcpDir), "the MCP key store, never the provider keys")
		base := path.Base(c.File)
		Expect(base).To(HavePrefix(forgecredfile.SealedPrefix + org.String() + "-" + agent.String() + "-"))

		// The agent's forge routes name the credential.
		named := false
		for _, rt := range mg.last().EgressRoutes {
			if rt.Agent == ref && rt.Credential == c.Name {
				named = true
			}
		}
		Expect(named).To(BeTrue())

		// The file is ciphertext: the token is not in it, in the provider keys or in the state.
		data := secretData(ctx)
		Expect(data).To(HaveKey(base))
		Expect(string(data[base])).NotTo(ContainSubstring("koinon_test_not-a-real-token"))
		providers, err := kube.CoreV1().Secrets(namespace).Get(ctx, "koinon-gateway-providers", metav1.GetOptions{})
		if err == nil {
			Expect(providers.Data).NotTo(HaveKey(base))
		}
		state, err := json.Marshal(mg.last())
		Expect(err).NotTo(HaveOccurred())
		Expect(string(state)).NotTo(ContainSubstring("koinon_test_not-a-real-token"))

		// The gateway's opener reads the token from the same file.
		dir := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(dir, base), data[base], 0o600)).To(Succeed())
		local := c
		local.File = filepath.Join(dir, base)
		v, err := egress.SealedCredentials{Keys: secretstore.NewRecipientKeyring(gk)}.Open(local)
		Expect(err).NotTo(HaveOccurred())
		Expect(v).To(Equal("koinon_test_not-a-real-token"))

		// An unchanged token keeps its file: no new file, no settle.
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(credOf(agent)).To(Equal(creds))
		n := 0
		for name := range secretData(ctx) {
			if strings.HasPrefix(name, forgecredfile.SealedPrefix+org.String()+"-") {
				n++
			}
		}
		Expect(n).To(Equal(1))
	})

	It("delivers no credential without a gateway key source, so the forge routes fail closed", func(ctx SpecContext) {
		r := newReconciler(nil)
		org, agent := readAgent(ctx)
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		Expect(credOf(agent)).To(BeEmpty())
		s, err := kube.CoreV1().Secrets(namespace).Get(ctx, mcpSecret, metav1.GetOptions{})
		if err == nil {
			for name := range s.Data {
				Expect(name).NotTo(HavePrefix(forgecredfile.SealedPrefix + org.String()))
			}
		}
	})
})
