// SPDX-License-Identifier: Apache-2.0

package drill

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/bcrypt"
)

// The drill's own OIDC provider (ADR 0109, amendment 2026-11-13): Dex in the fakes namespace, like the fake model.
// The check `oidc-signin` signs in through it, so it depends on no external identity provider, no secret store and
// no state that outlives a run.
const (
	// FakeIdPName is the Deployment, the Service and the Secret of the provider.
	FakeIdPName = "dex"
	// FakeIdPPort is the port the Service and the pod listen on.
	FakeIdPPort = 5556
	// FakeIdPClientID is the one static client, a public one: PKCE is enforced and there is no client secret.
	FakeIdPClientID = "koinon-drill"
	// FakeIdPPassword is the password of every drill user. Obviously fake; Dex gets a bcrypt hash of it that is
	// made at run time, so no hash is in the repository.
	FakeIdPPassword = "koinon_test_not-a-real-password"
	// FakeIdPForwardPort is the fixed local port of the drill host's port-forward to the provider. Chromium needs a
	// fixed one for --host-resolver-rules; the Go client could take any.
	FakeIdPForwardPort = 18556
)

// The IdP groups of the drill users, each mapped to a role in the default org by the chart overlay.
const (
	GroupAdmins = "koinon-admins"
	GroupUsers  = "koinon-users"
)

// IdPUser is one static user of the drill's provider.
type IdPUser struct {
	// Username is the Dex username (the `name` claim). The login form asks for the e-mail address, see Email.
	Username string
	// Group is the single IdP group of the user.
	Group string
	// Role is the role in the default org that the group maps to (the overlay's groupMappings).
	Role string
	// Disabled marks the user the drill disables in Koinon before the backup.
	Disabled bool
}

// The persona names of the drill users.
const (
	UserAdmin    = "drill-admin"
	UserMember   = "drill-member"
	UserDisabled = "drill-disabled"
)

// IdPUsers are the three users of the drill's provider, in the order the check runs them.
func IdPUsers() []IdPUser {
	return []IdPUser{
		{Username: UserAdmin, Group: GroupAdmins, Role: "admin"},
		{Username: UserMember, Group: GroupUsers, Role: "member"},
		{Username: UserDisabled, Group: GroupUsers, Role: "member", Disabled: true},
	}
}

// Email is the user's e-mail address (the `email` claim).
func (u IdPUser) Email() string { return u.Username + "@drill.invalid" }

// FakeIdPHost is the provider's name inside the cluster. koinon-api reaches the provider by it; the drill host
// maps it to its port-forward (the Go client in its dialer, Chromium with --host-resolver-rules).
func FakeIdPHost(fakesNS string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", FakeIdPName, fakesNS)
}

// FakeIdPIssuer is the issuer URL: the same string for koinon-api, the Go client and the browser, so the `iss`
// claim and the redirects match everywhere. The scheme is http: the provider is in-cluster and the drill's only
// client of it is Koinon (internal/auth/oidctest uses an http issuer the same way).
func FakeIdPIssuer(fakesNS string) string {
	return fmt.Sprintf("http://%s:%d/dex", FakeIdPHost(fakesNS), FakeIdPPort)
}

// FakeIdPManifest is the drill's OIDC provider: a Secret with Dex's config, a Deployment and a Service in the fakes
// namespace, which the caller has made. The config has storage in memory, the password database with the given
// users, one public static client with PKCE enforced and the drill's two loopback redirect URLs, and no approval
// screen. The password hashes are bcrypt hashes of FakeIdPPassword made here, at run time.
func FakeIdPManifest(ns, runID, image string, users []IdPUser) (string, error) {
	cfg, err := fakeIdPConfig(ns, users)
	if err != nil {
		return "", err
	}
	// The config is a block scalar in the Secret, so the YAML of the config needs no escaping.
	indented := "    " + strings.ReplaceAll(strings.TrimRight(cfg, "\n"), "\n", "\n    ")
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %[5]s-config
  namespace: %[1]s
  labels:
    %[3]s: "%[2]s"
stringData:
  config.yaml: |
%[6]s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[5]s
  namespace: %[1]s
  labels: {app: %[5]s, %[3]s: "%[2]s"}
spec:
  replicas: 1
  selector:
    matchLabels: {app: %[5]s}
  template:
    metadata:
      labels: {app: %[5]s, %[3]s: "%[2]s"}
    spec:
      automountServiceAccountToken: false
      containers:
        - name: dex
          image: %[4]s
          imagePullPolicy: IfNotPresent
          args: ["dex", "serve", "/etc/dex/config.yaml"]
          ports:
            - {name: http, containerPort: %[7]d}
          readinessProbe:
            httpGet: {path: /dex/.well-known/openid-configuration, port: http}
          volumeMounts:
            - {name: config, mountPath: /etc/dex, readOnly: true}
            # The image's entrypoint writes a temporary copy of the config.
            - {name: tmp, mountPath: /tmp}
          securityContext:
            runAsNonRoot: true
            runAsUser: 1001
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
      volumes:
        - name: config
          secret: {secretName: %[5]s-config}
        - name: tmp
          emptyDir: {medium: Memory, sizeLimit: 8Mi}
---
apiVersion: v1
kind: Service
metadata:
  name: %[5]s
  namespace: %[1]s
  labels:
    %[3]s: "%[2]s"
spec:
  selector: {app: %[5]s}
  ports:
    - {name: http, port: %[7]d, targetPort: http}
`, ns, runID, RunLabel, image, FakeIdPName, indented, FakeIdPPort), nil
}

// fakeIdPConfig renders Dex's config.yaml.
func fakeIdPConfig(ns string, users []IdPUser) (string, error) {
	if len(users) == 0 {
		return "", fmt.Errorf("the provider needs at least one user")
	}
	// The hash is made once: bcrypt salts it, and every user has the same fake password anyway. Cost 10 (the
	// default) keeps a login quick and the hash a real bcrypt one.
	hash, err := bcrypt.GenerateFromPassword([]byte(FakeIdPPassword), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash the drill password: %w", err)
	}
	type password struct {
		Email    string   `yaml:"email"`
		Hash     string   `yaml:"hash"`
		Username string   `yaml:"username"`
		UserID   string   `yaml:"userID"`
		Groups   []string `yaml:"groups"`
	}
	var pws []password
	for _, u := range users {
		pws = append(pws, password{Email: u.Email(), Hash: string(hash), Username: u.Username, UserID: "koinon-drill-" + u.Username, Groups: []string{u.Group}})
	}
	cfg := map[string]any{
		"issuer":  FakeIdPIssuer(ns),
		"storage": map[string]any{"type": "memory"},
		"web":     map[string]any{"http": fmt.Sprintf("0.0.0.0:%d", FakeIdPPort)},
		// The image keeps its templates and static files here; the default is a path relative to the working directory.
		"frontend": map[string]any{"dir": "/srv/dex/web"},
		"oauth2": map[string]any{
			// Koinon sends no prompt parameter, so no approval page: a person would see none either.
			"skipApprovalScreen": true,
			// Koinon's login uses S256 (internal/auth/oidc.go); a client that sends none is refused.
			"pkce": map[string]any{"enforce": true, "codeChallengeMethodsSupported": []string{"S256"}},
		},
		"enablePasswordDB": true,
		"staticPasswords":  pws,
		"staticClients": []any{map[string]any{
			"id": FakeIdPClientID, "name": "Koinon drill", "public": true,
			"redirectURIs": []string{OIDCRedirect(OIDCSourcePort), OIDCRedirect(OIDCTargetPort)},
		}},
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
