// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	"github.com/stateracloud/koinon/internal/forgecredfile"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/egress"
	"github.com/stateracloud/koinon/internal/gateway/route"
	"github.com/stateracloud/koinon/internal/gateway/staterev"
	gwv1 "github.com/stateracloud/koinon/internal/gen/koinon/gateway/v1alpha1"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/testing/racescale"
	"github.com/stateracloud/koinon/pricing"
	"github.com/stateracloud/koinon/templates"
)

// staticRecipient is the gateway's public key, as gatewaykeys.PublicKeys gives it.
type staticRecipient struct {
	pub secretstore.RecipientPublicKey
}

func (s staticRecipient) Current() (secretstore.RecipientPublicKey, error) { return s.pub, nil }

// rotatingRecipient is a gateway key source whose key the spec can change between passes.
type rotatingRecipient struct {
	pub *secretstore.RecipientPublicKey
}

func (s rotatingRecipient) Current() (secretstore.RecipientPublicKey, error) { return *s.pub, nil }

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

	// newReconcilerWith builds a reconciler with the gateway key source (none when recipient is nil), the
	// code-host token minter and koinon-gateway's egress listener; opts change the config last.
	newReconcilerWith := func(recipient reconciler.RecipientKeys, tokens reconciler.GitHubTokens, opts ...func(*reconciler.Config)) *reconciler.Reconciler {
		GinkgoHelper()
		osg := fakegateway.New()
		cc, err := osg.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = cc.Close(); osg.Stop() })
		cfg := reconciler.Config{
			KeepWarmPerOrg: 2,
			Prices:         prices.NewResolver(pricing.Embedded()),
			Gateway:        reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080},
			Egress:         &reconciler.Endpoint{Host: "koinon-mcp-gateway.koinon.svc", Port: 8081},
			Keys:           ring,
			ProviderKeys:   reconciler.KubeProviderKeys{Client: kube, Namespace: namespace, Name: "koinon-gateway-providers"},
			ProviderKeyDir: "/secrets/providers",
			MCPKeys:        reconciler.KubeProviderKeys{Client: kube, Namespace: namespace, Name: mcpSecret},
			MCPKeyDir:      mcpDir, MCPSealed: true, ForgeRecipient: recipient,
			Harnesses: map[string]reconciler.Harness{
				harness.OpenCode: {Image: image, Binaries: reconciler.DefaultBinaries[harness.OpenCode]},
			},
			GitHub: tokens, URLPolicy: testPolicy,
			MCPAckWait: racescale.D(300 * time.Millisecond), MCPAckPoll: 20 * time.Millisecond,
			Logger: slog.New(slog.NewTextHandler(&syncBuffer{}, nil)),
		}
		for _, o := range opts {
			o(&cfg)
		}
		r, err := reconciler.New(pool, openshell.New(cc), mg, reconciler.KubeSecrets{Client: kube, Namespace: namespace}, cfg)
		Expect(err).NotTo(HaveOccurred())
		return r
	}
	newReconciler := func(recipient reconciler.RecipientKeys) *reconciler.Reconciler {
		GinkgoHelper()
		return newReconcilerWith(recipient, fixedTokens{})
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
		Expect(fmt.Sprintf("%#v", mg.last())).NotTo(ContainSubstring("koinon_test_not-a-real-token"))

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

	It("seals the token again when the gateway key changes: the new key opens the new file, the old key does not", func(ctx SpecContext) {
		pub := gk.Public()
		r := newReconciler(rotatingRecipient{pub: &pub})
		_, agent := readAgent(ctx)
		Expect(r.ReconcileAll(ctx)).To(Succeed())
		before := credOf(agent)
		Expect(before).To(HaveLen(1))

		gk2, err := secretstore.GenerateRecipientKey(secretstore.DefaultRecipientSuite())
		Expect(err).NotTo(HaveOccurred())
		pub = gk2.Public()

		// The new file settles before the state names it, so allow a few passes.
		var after []route.Credential
		for range 3 {
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			after = credOf(agent)
			if len(after) == 1 && after[0].File != before[0].File {
				break
			}
		}
		Expect(after).To(HaveLen(1))
		Expect(after[0].File).NotTo(Equal(before[0].File), "a new gateway key gives a new envelope and a new file")

		base := path.Base(after[0].File)
		data := secretData(ctx)
		Expect(data).To(HaveKey(base))
		dir := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(dir, base), data[base], 0o600)).To(Succeed())
		local := after[0]
		local.File = filepath.Join(dir, base)

		v, err := egress.SealedCredentials{Keys: secretstore.NewRecipientKeyring(gk2)}.Open(local)
		Expect(err).NotTo(HaveOccurred())
		Expect(v).To(Equal("koinon_test_not-a-real-token"))
		_, err = egress.SealedCredentials{Keys: secretstore.NewRecipientKeyring(gk)}.Open(local)
		Expect(err).To(HaveOccurred(), "the old gateway key cannot open the resealed token")
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
		row, err := q.GetGatewayAgentForge(ctx, store.GetGatewayAgentForgeParams{OrgID: org, AgentID: agent})
		Expect(err).NotTo(HaveOccurred(), "the agent expects a credential, so its runs fail with the cause")
		Expect(row.Failure).To(ContainSubstring("no key source for gateway credentials"))
	})

	Describe("the record that koinon-agentd waits on before the harness starts (#1086 part C2)", func() {
		forgeRow := func(ctx SpecContext, org, agent pgtype.UUID) (store.GetGatewayAgentForgeRow, error) {
			return q.GetGatewayAgentForge(ctx, store.GetGatewayAgentForgeParams{OrgID: org, AgentID: agent})
		}

		It("records the revision that holds the credential; with no live copy it is served; an archived agent has no row", func(ctx SpecContext) {
			r := newReconciler(staticRecipient{pub: gk.Public()})
			org, agent := readAgent(ctx)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			Expect(credOf(agent)).To(HaveLen(1))
			row, err := forgeRow(ctx, org, agent)
			Expect(err).NotTo(HaveOccurred())
			latest, err := staterev.Latest(ctx, pool)
			Expect(err).NotTo(HaveOccurred())
			Expect(row.StateRevision.Valid).To(BeTrue())
			Expect(row.StateRevision.Int64).To(BeNumerically("<=", latest.Revision))
			Expect(row.Failure).To(BeEmpty())
			Expect(row.ServedAt.Valid).To(BeTrue(), "no copy serves from the stream, so nothing has to ack")

			_, err = q.ArchiveAgent(ctx, store.ArchiveAgentParams{OrgID: org, ID: agent})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			_, err = forgeRow(ctx, org, agent)
			Expect(err).To(MatchError(pgx.ErrNoRows), "an agent that expects no credential has no row")
		})

		It("writes no row for an agent without a forge read grant, so its runs start at once", func(ctx SpecContext) {
			r := newReconciler(staticRecipient{pub: gk.Public()})
			org, user := newOrg(ctx)
			a := newAgent(ctx, org, user)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			_, err := forgeRow(ctx, org, a.ID)
			Expect(err).To(MatchError(pgx.ErrNoRows))
		})

		It("writes no row when the install has no egress listener: git then uses the driver's own rule", func(ctx SpecContext) {
			r := newReconcilerWith(staticRecipient{pub: gk.Public()}, fixedTokens{}, func(c *reconciler.Config) { c.Egress = nil })
			org, agent := readAgent(ctx)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			_, err := forgeRow(ctx, org, agent)
			Expect(err).To(MatchError(pgx.ErrNoRows))
		})

		It("records why the reconciler cannot make the credential, and never the token", func(ctx SpecContext) {
			r := newReconcilerWith(staticRecipient{pub: gk.Public()}, failingTokens{})
			org, agent := readAgent(ctx)
			Expect(r.ReconcileAll(ctx)).To(MatchError(ContainSubstring("koinon_test_installation_suspended")), "the pass reports the failed mint too")
			Expect(credOf(agent)).To(BeEmpty(), "the forge routes fail closed")
			row, err := forgeRow(ctx, org, agent)
			Expect(err).NotTo(HaveOccurred())
			Expect(row.Failure).To(ContainSubstring("mint the code-host token"))
			Expect(row.Failure).To(ContainSubstring("koinon_test_installation_suspended"))
			Expect(row.StateRevision.Valid).To(BeFalse())
			Expect(row.ServedAt.Valid).To(BeFalse())
		})

		It("is served only when every live copy acked the revision: one copy of two lags", func(ctx SpecContext) {
			const copyA, copyB = "koinon-gateway-test/boot-forge-a", "koinon-gateway-test/boot-forge-b"
			for _, c := range []string{copyA, copyB} {
				Expect(staterev.TouchCopy(ctx, pool, c, "test-stream", true)).To(Succeed())
				DeferCleanup(func() { _ = staterev.ForgetCopy(context.Background(), pool, c, "test-stream") })
			}
			r := newReconciler(staticRecipient{pub: gk.Public()})
			org, agent := readAgent(ctx)
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			row, err := forgeRow(ctx, org, agent)
			Expect(err).NotTo(HaveOccurred())
			Expect(row.StateRevision.Valid).To(BeTrue())
			Expect(row.ServedAt.Valid).To(BeFalse(), "no copy acked")

			rev := row.StateRevision.Int64
			Expect(staterev.Ack(ctx, pool, copyA, rev, gwv1.Half_HALF_MCP)).To(Succeed())
			Expect(staterev.Ack(ctx, pool, copyB, rev, gwv1.Half_HALF_MODEL)).To(Succeed())
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			row, err = forgeRow(ctx, org, agent)
			Expect(err).NotTo(HaveOccurred())
			Expect(row.ServedAt.Valid).To(BeFalse(), "copy B acked only the model half")

			Expect(staterev.Ack(ctx, pool, copyB, rev, gwv1.Half_HALF_MCP)).To(Succeed())
			Expect(r.ReconcileAll(ctx)).To(Succeed())
			row, err = forgeRow(ctx, org, agent)
			Expect(err).NotTo(HaveOccurred())
			Expect(row.ServedAt.Valid).To(BeTrue())
			Expect(row.StateRevision.Int64).To(Equal(rev), "the revision that first held the credential stays")
		})
	})
})

// failingTokens is a code-host token minter that always fails, as GitHub does for a suspended installation.
type failingTokens struct{}

func (failingTokens) ReadToken(context.Context, store.Connection, codehost.Scope) (codehost.Token, error) {
	return codehost.Token{}, errors.New("koinon_test_installation_suspended")
}
