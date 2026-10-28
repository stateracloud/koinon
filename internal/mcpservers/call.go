// SPDX-License-Identifier: Apache-2.0

package mcpservers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Approved ask-tier calls are made through the gateway (ADR 0057 amendment), with a per-org executor
// key: the gateway's egress policy, URL checks and credential injection apply to them as to an
// agent's calls, and koinon-api never holds or sends a server credential.

// MaxResultBytes bounds the result of an approved call that is stored and shown to the agent.
const MaxResultBytes = 64 << 10

// Errors of CallTool, by what is known about the call.
var (
	// ErrNotSent: the failure came before the tools/call request was sent (connecting, initialize), so
	// the call did not happen and may be tried again.
	ErrNotSent = errors.New("the call was not sent")
	// ErrOutcomeUnknown: the tools/call request was sent and the answer did not come. The call may or
	// may not have happened; it is never retried.
	ErrOutcomeUnknown = errors.New("outcome unknown: the call was sent and no answer came back; Koinon does not retry it, so check the server before asking again")
	// ErrRefused: the gateway or server answered with a protocol error (an unknown tool, invalid
	// arguments): the call did not happen.
	ErrRefused = errors.New("the gateway or the server refused the call")
)

// CallRequest is one approved tool call.
type CallRequest struct {
	// GatewayMCPURL is the gateway's MCP endpoint, for example http://koinon-gateway.koinon.svc:8080/mcp.
	GatewayMCPURL string
	// Key is the org's executor key.
	Key string
	// Target is the server's gateway target and Tool the server's own name for the tool.
	Target, Tool string
	// Arguments are sent exactly as given (the approved, canonical JSON), not decoded and re-encoded.
	Arguments json.RawMessage
	// Idempotency is passed to the server as _meta "dev.koinon/approval".
	Idempotency string
}

type bearer struct {
	key  string
	base http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return b.base.RoundTrip(r)
}

// CallTool calls the tool through the gateway. An error the tool reports is a result (isError), not a
// failure of the call. The result's content is bounded by MaxResultBytes.
func CallTool(ctx context.Context, c CallRequest) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "koinon-executor", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             c.GatewayMCPURL,
		HTTPClient:           &http.Client{Transport: bearer{key: c.Key, base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: connect to the gateway: %w", ErrNotSent, err)
	}
	defer func() { _ = session.Close() }()
	params := &mcp.CallToolParams{Name: c.Target + "_" + c.Tool}
	if len(c.Arguments) > 0 {
		params.Arguments = c.Arguments // json.RawMessage: marshalled as is, so 1.0 and big integers survive
	}
	if c.Idempotency != "" {
		params.Meta = mcp.Meta{"dev.koinon/approval": c.Idempotency}
	}
	res, err := session.CallTool(ctx, params)
	if err != nil {
		// Only the standard "I did not run this" answers are a refusal; anything else after the
		// request left (a dropped connection, a transport error) may have run the call.
		var werr *jsonrpc.Error
		if errors.As(err, &werr) && (werr.Code == -32601 || werr.Code == -32602 || werr.Code == -32600) {
			return nil, fmt.Errorf("%w: %s", ErrRefused, werr.Message)
		}
		return nil, fmt.Errorf("%w (%w)", ErrOutcomeUnknown, err)
	}
	out := map[string]any{"isError": res.IsError}
	raw, err := json.Marshal(res.Content)
	if err != nil {
		return nil, fmt.Errorf("%w: unreadable result", ErrOutcomeUnknown)
	}
	if len(raw) > MaxResultBytes {
		out["truncated"] = true
		out["content"] = []map[string]string{{"type": "text", "text": fmt.Sprintf("The result is %d bytes; Koinon keeps only the first %d.", len(raw), MaxResultBytes/2)}, {"type": "text", "text": string(raw[:MaxResultBytes/2])}}
	} else {
		var content any
		_ = json.Unmarshal(raw, &content)
		out["content"] = content
	}
	if res.StructuredContent != nil {
		if sc, err := json.Marshal(res.StructuredContent); err == nil && len(sc) <= MaxResultBytes {
			var v any
			_ = json.Unmarshal(sc, &v)
			out["structuredContent"] = v
		}
	}
	return out, nil
}

// URLHash identifies a server's URL in an approval's binding.
func URLHash(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])
}

// CallBinding is what an MCP approval request is bound to when the proxy records it. The executor
// refuses the call if any of it has changed.
type CallBinding struct {
	ServerID    string `json:"serverId"`
	URLHash     string `json:"urlHash"`
	ContentHash string `json:"contentHash"`
}

// ExecSecretName is the Secret that holds an org's executor key: the reconciler writes it and
// koinon-api reads it. The key opens only that org's tools through the gateway.
func ExecSecretName(org pgtype.UUID) string {
	sum := sha256.Sum256([]byte("mcp-exec\x00" + org.String()))
	return "koinon-mcp-exec-" + hex.EncodeToString(sum[:8])
}

// ModelCheckSecretName is the Secret that holds an org's model-check key (the provider echo check,
// gateway.ModelCheckAgentName): the reconciler writes it and koinon-api reads it. The key requests
// only the org's served, priced models.
func ModelCheckSecretName(org pgtype.UUID) string {
	sum := sha256.Sum256([]byte("model-check\x00" + org.String()))
	return "koinon-model-check-" + hex.EncodeToString(sum[:8])
}

// ExecKeyDataKey is the Secret's data key (the reconciler's key Secrets use it too).
const ExecKeyDataKey = "api_key"

// KubeExecKeys reads an org's gateway key from a Secret in one namespace: the executor key, or with
// SecretName set another per-org system key (ModelCheckSecretName).
type KubeExecKeys struct {
	Client    kubernetes.Interface
	Namespace string
	// SecretName names the org's Secret; nil is ExecSecretName.
	SecretName func(org pgtype.UUID) string
}

// Key returns the org's key.
func (k KubeExecKeys) Key(ctx context.Context, org pgtype.UUID) (string, error) {
	name := ExecSecretName
	if k.SecretName != nil {
		name = k.SecretName
	}
	s, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, name(org), metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read the org's key from the Secret %s: %w", name(org), err)
	}
	if len(s.Data[ExecKeyDataKey]) == 0 {
		return "", fmt.Errorf("the Secret %s holds no key", name(org))
	}
	return string(s.Data[ExecKeyDataKey]), nil
}
