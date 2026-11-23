// SPDX-License-Identifier: Apache-2.0

package mcpgateway_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpcredfile"
	"github.com/stateracloud/koinon/internal/mcpgateway"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// syncBuffer is a log sink that many goroutines write to.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// leakForms are the forms of fakeToken that must never be emitted: the value, as the gateway sends it, and
// encoded the way a log or a span may print it.
func leakForms() []string {
	return []string{
		fakeToken,
		base64.StdEncoding.EncodeToString([]byte(fakeToken)),
		base64.RawURLEncoding.EncodeToString([]byte(fakeToken)),
	}
}

// spanTexts returns every text of the recorded spans: names, attribute keys and values, status, events, links.
func spanTexts(rec *tracetest.SpanRecorder) []string {
	var out []string
	kvs := func(attrs []attribute.KeyValue) {
		for _, kv := range attrs {
			out = append(out, string(kv.Key), kv.Value.String())
		}
	}
	for _, s := range rec.Ended() {
		out = append(out, s.Name(), s.Status().Description)
		kvs(s.Attributes())
		for _, e := range s.Events() {
			out = append(out, e.Name)
			kvs(e.Attributes)
		}
		for _, l := range s.Links() {
			kvs(l.Attributes)
		}
	}
	return out
}

func expectNoLeak(what string, texts ...string) {
	GinkgoHelper()
	for _, t := range texts {
		for _, f := range leakForms() {
			Expect(t).NotTo(ContainSubstring(f), "%s holds a form of the credential", what)
		}
	}
}

var _ = Describe("CredentialScrub", func() {
	var (
		sc  *mcpgateway.CredentialScrub
		buf *syncBuffer
		log *slog.Logger
		rec *tracetest.SpanRecorder
		tr  trace.Tracer
	)
	BeforeEach(func() {
		sc = mcpgateway.NewCredentialScrub()
		buf = &syncBuffer{}
		log = slog.New(sc.Handler(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		rec = tracetest.NewSpanRecorder()
		tr = sc.Tracer(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)).Tracer("t"))
	})

	It("scrubs a log line: message, attributes, errors, Stringers, groups and attributes bound before the open", func() {
		bound := log.With("early", "bound "+fakeToken).WithGroup("g-" + fakeToken)
		sc.Add("Bearer " + fakeToken)
		log.Info("msg "+fakeToken, "plain", fakeToken, "b64", base64.StdEncoding.EncodeToString([]byte(fakeToken)),
			"err", fmt.Errorf("upstream said %s", fakeToken), "any", struct{ V string }{fakeToken},
			slog.Group("grp", "inner", "x"+fakeToken), fakeToken, "key")
		bound.Warn("later", "v", fakeToken)
		out := buf.String()
		expectNoLeak("the log", out)
		Expect(out).To(ContainSubstring("[redacted:secret]"))
		Expect(out).To(ContainSubstring(`"plain":"[redacted:secret]"`))
		Expect(strings.Count(out, "\n")).To(Equal(2), "both lines are written")
	})

	It("keeps a log value that holds no credential in its own form", func() {
		sc.Add(fakeToken)
		log.Info("ok", "n", 3, "any", map[string]int{"a": 1})
		Expect(buf.String()).To(ContainSubstring(`"n":3`))
		Expect(buf.String()).To(ContainSubstring(`"any":{"a":1}`))
	})

	It("scrubs a span: name, start attributes, attributes, status, events, recorded errors, links, and the span in the context", func(ctx SpecContext) {
		sc.Add(fakeToken)
		ctx2, sp := tr.Start(ctx, "call "+fakeToken, trace.WithAttributes(attribute.String("start", fakeToken)),
			trace.WithLinks(trace.Link{Attributes: []attribute.KeyValue{attribute.String("link", fakeToken)}}))
		sp.SetAttributes(attribute.String("a", "x "+fakeToken), attribute.StringSlice("s", []string{fakeToken}), attribute.Int("n", 1))
		sp.SetStatus(codes.Error, "failed: "+fakeToken)
		sp.AddEvent("ev "+fakeToken, trace.WithAttributes(attribute.String("e", fakeToken)))
		sp.RecordError(errors.New("boom "+fakeToken), trace.WithAttributes(attribute.String("r", fakeToken)))
		sp.SetName("renamed " + fakeToken)
		trace.SpanFromContext(ctx2).SetAttributes(attribute.String("fromctx", fakeToken))
		_, child := trace.SpanFromContext(ctx2).TracerProvider().Tracer("c").Start(ctx2, "child "+fakeToken)
		child.End()
		sp.End()
		texts := spanTexts(rec)
		Expect(texts).NotTo(BeEmpty())
		expectNoLeak("a span", texts...)
		Expect(rec.Ended()).To(HaveLen(2))
	})

	It("scrubs an error and leaves one without a credential as it is", func() {
		sc.Add(fakeToken)
		base := errors.New("no value here")
		Expect(sc.Error(base)).To(BeIdenticalTo(base))
		err := sc.Error(fmt.Errorf("wrapped: %w", errors.New(fakeToken)))
		Expect(err.Error()).To(Equal("wrapped: [redacted:secret]"))
		Expect(errors.Unwrap(err)).To(BeNil(), "an Unwrap would give the value back")
	})

	It("matches the token after an auth scheme and the decoded parts of Basic", func() {
		basic := base64.StdEncoding.EncodeToString([]byte("koinon_test_user:" + fakeToken))
		sc.Add("Basic " + basic)
		Expect(sc.Text("p=" + fakeToken)).To(Equal("p=[redacted:secret]"))
		Expect(sc.Text(basic)).To(Equal("[redacted:secret]"))
		Expect(sc.Text("user=koinon_test_user")).To(Equal("user=koinon_test_user"), "a Basic user name is not a secret")
	})

	It("ignores a value too short to match without redacting ordinary words", func() {
		sc.Add("short", "")
		Expect(sc.Text("a short word")).To(Equal("a short word"))
	})

	It("drops a value not opened within Retain and caps the set", func() {
		now := time.Unix(1_700_000_000, 0)
		mcpgateway.SetScrubClock(sc, func() time.Time { return now })
		sc.Retain = time.Hour
		sc.Add(fakeToken)
		Expect(sc.Text(fakeToken)).To(Equal("[redacted:secret]"))
		now = now.Add(30 * time.Minute)
		sc.Add("koinon_test_other_value")
		Expect(sc.Text(fakeToken)).To(Equal("[redacted:secret]"), "opened within Retain")
		now = now.Add(2 * time.Hour)
		sc.Add("koinon_test_third_value")
		Expect(sc.Text(fakeToken)).To(Equal(fakeToken), "not opened within Retain")
		Expect(sc.Text("koinon_test_third_value")).To(Equal("[redacted:secret]"))
		capped := mcpgateway.NewCredentialScrub()
		capped.MaxValues = 2
		capped.Add("koinon_test_value_one", "koinon_test_value_two", "koinon_test_value_three")
		n := 0
		for _, v := range []string{"koinon_test_value_one", "koinon_test_value_two", "koinon_test_value_three"} {
			if capped.Text(v) != v {
				n++
			}
		}
		Expect(n).To(Equal(2))
	})

	It("scrubs every field of a tool result: text, resources, links, blobs, metadata and structured content", func() {
		sc.Add("Bearer " + fakeToken)
		res := sc.Result(&mcp.CallToolResult{
			Meta: mcp.Meta{"echo": fakeToken},
			Content: []mcp.Content{
				&mcp.TextContent{Text: "got " + fakeToken, Meta: mcp.Meta{"t": fakeToken}},
				&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "x:" + fakeToken, Text: fakeToken}},
				&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "x:y", Blob: []byte("hdr=Bearer " + fakeToken)}},
				&mcp.ResourceLink{URI: "https://example.invalid/?t=" + fakeToken, Name: fakeToken},
				&mcp.ImageContent{MIMEType: "image/png", Data: []byte("png " + fakeToken)},
			},
			StructuredContent: map[string]any{"token": fakeToken, "n": 1},
		})
		raw, err := json.Marshal(res)
		Expect(err).NotTo(HaveOccurred())
		expectNoLeak("the result", string(raw))
		Expect(res.Content).To(HaveLen(5))
		Expect(res.StructuredContent).To(HaveKeyWithValue("n", BeNumerically("==", 1)))
		Expect(res.Content[2].(*mcp.EmbeddedResource).Resource.Blob).To(Equal([]byte("hdr=[redacted:secret]")))
	})

	It("scrubs a tool list: each tool's text, schema and metadata, and leaves a clean tool as it is", func() {
		sc.Add(fakeToken)
		clean := &mcp.Tool{Name: "clean", Description: "fine"}
		res := sc.ToolList(&mcp.ListToolsResult{
			Meta: mcp.Meta{"m": fakeToken},
			Tools: []*mcp.Tool{clean, {
				Name: "t", Title: fakeToken, Description: "uses " + fakeToken,
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{
					"k": map[string]any{"type": "string", "default": fakeToken, "enum": []any{fakeToken}},
				}},
				Meta: mcp.Meta{"x": fakeToken},
			}},
		})
		raw, err := json.Marshal(res)
		Expect(err).NotTo(HaveOccurred())
		expectNoLeak("the tool list", string(raw))
		Expect(res.Tools).To(HaveLen(2))
		Expect(res.Tools[0]).To(BeIdenticalTo(clean))
		Expect(res.Tools[1].Description).To(Equal("uses [redacted:secret]"))
	})

	It("returns a tool result with no credential unchanged", func() {
		sc.Add(fakeToken)
		in := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "fine"}}}
		Expect(sc.Result(in)).To(BeIdenticalTo(in))
	})

	It("scrubs a []byte log value, which a text handler writes as a string", func() {
		text := &syncBuffer{}
		tlog := slog.New(sc.Handler(slog.NewTextHandler(text, nil)))
		sc.Add(fakeToken)
		tlog.Info("x", "b", []byte("hdr="+fakeToken), "raw", json.RawMessage(`{"t":"`+fakeToken+`"}`))
		expectNoLeak("the text log", text.String())
		Expect(text.String()).To(ContainSubstring("[redacted:secret]"))
	})

	It("does not print its values", func() {
		sc.Add(fakeToken)
		Expect(fmt.Sprintf("%v %+v %#v %s", sc, sc, sc, sc)).NotTo(ContainSubstring(fakeToken))
	})
})

// The done-when of #1174: a request goes through koinon-gateway with a sealed fake credential, the target echoes
// it in every place it can, and no exported span, log line, audit row or result to the agent holds it.
var _ = Describe("the gateway scrubs the credential it opened (#1174)", func() {
	const org = "11111111-1111-4111-8111-111111111111"
	const server = "22222222-2222-4222-8222-222222222222"
	agent := gateway.AgentRef{Org: "acme", Name: "scrub-reader"}

	// leakyUpstream is a target that quotes the credential it was sent, in clear and encoded, in a result, in
	// structured content and in a tool error.
	leakyUpstream := func() *httptest.Server {
		newServer := func(r *http.Request) *mcp.Server {
			got := r.Header.Get("Authorization")
			tok := strings.TrimPrefix(got, "Bearer ")
			s := mcp.NewServer(&mcp.Implementation{Name: "leaky", Version: "0"}, nil)
			mcp.AddTool(s, &mcp.Tool{Name: "echo", Title: "echo " + tok, Description: "the target got " + got}, func(_ context.Context, _ *mcp.CallToolRequest, a echoArgs) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{
					Content: []mcp.Content{
						&mcp.TextContent{Text: a.Text + " header=" + got},
						&mcp.TextContent{Text: "b64=" + base64.StdEncoding.EncodeToString([]byte(tok))},
					},
					StructuredContent: map[string]any{"token": tok},
				}, nil, nil
			})
			mcp.AddTool(s, &mcp.Tool{Name: "fail"}, func(context.Context, *mcp.CallToolRequest, echoArgs) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "401: bad token " + tok}}}, nil, nil
			})
			return s
		}
		return httptest.NewServer(mcp.NewStreamableHTTPHandler(newServer, nil))
	}

	It("removes the value from spans, log lines, the audit row and the agent's results, on success and error paths", func(ctx SpecContext) {
		gk, err := secretstore.GenerateRecipientKey(secretstore.DefaultRecipientSuite())
		Expect(err).NotTo(HaveOccurred())
		var o, srvID pgtype.UUID
		Expect(o.Scan(org)).To(Succeed())
		Expect(srvID.Scan(server)).To(Succeed())
		env, err := secretstore.SealV2(gk.Public(), secretstore.Binding{OrgID: org, Table: mcpcredfile.ServersTable, RowID: server, Column: mcpcredfile.CredentialColumn}, []byte(fakeToken))
		Expect(err).NotTo(HaveOccurred())
		raw, err := json.Marshal(env)
		Expect(err).NotTo(HaveOccurred())
		credFile := filepath.Join(GinkgoT().TempDir(), mcpcredfile.SealedName(o, srvID, raw))
		Expect(os.WriteFile(credFile, raw, 0o600)).To(Succeed())

		up := leakyUpstream()
		DeferCleanup(func() { up.CloseClientConnections(); up.Close() })

		// The wiring of koinon-gateway's run: one scrub for the process logger and the gateway.
		sc := mcpgateway.NewCredentialScrub()
		logs := &syncBuffer{}
		log := slog.New(sc.Handler(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		rec := tracetest.NewSpanRecorder()
		audit := &loggingAudit{log: log}
		guard := &recordingGuard{refuse: map[string]string{"refuse-me": "refused: the text quotes " + fakeToken}}
		gw := mcpgateway.New(mcpgateway.Options{
			Guard:       guard,
			Audit:       audit,
			Logger:      log,
			Scrub:       sc,
			Tracer:      sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)).Tracer("koinon-gateway"),
			Credentials: mcpgateway.StaticCredentials{Keys: secretstore.NewRecipientKeyring(gk)},
		})
		DeferCleanup(gw.Drain)
		mux := http.NewServeMux()
		mux.Handle("/mcp", gw.Handler())
		front := httptest.NewServer(mux)
		DeferCleanup(func() { front.CloseClientConnections(); front.Close() })
		key := mustKey(agent)
		_, err = gw.ApplyMCP(ctx, gateway.MCPConfig{
			Keys: []gateway.Key{key.Key},
			MCPState: gateway.MCPState{
				MCP: []gateway.MCPTarget{{
					Name: "leaky", Org: "acme", URL: up.URL + "/mcp",
					CredentialHeader: "Authorization", CredentialPrefix: "Bearer ", CredentialFile: credFile,
				}},
				Tools: map[gateway.AgentRef][]gateway.ToolGrant{agent: {{Target: "leaky", Tools: []string{"echo", "fail"}}}},
			},
		})
		Expect(err).NotTo(HaveOccurred())
		s := connect(ctx, front.URL, key.Material)

		list, err := s.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		listed, err := json.Marshal(list)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(listed)).To(ContainSubstring("leaky_echo"))
		Expect(string(listed)).To(ContainSubstring("the target got [redacted:secret]"))
		expectNoLeak("the tool list to the agent", string(listed))

		var results []string
		call := func(tool, arg string) *mcp.CallToolResult {
			GinkgoHelper()
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "leaky_" + tool, Arguments: map[string]any{"text": arg}})
			Expect(err).NotTo(HaveOccurred())
			b, err := json.Marshal(res)
			Expect(err).NotTo(HaveOccurred())
			results = append(results, string(b))
			return res
		}
		ok := call("echo", "x")
		Expect(ok.IsError).To(BeFalse(), text(ok))
		Expect(text(ok)).To(ContainSubstring("header=[redacted:secret]"), "the target got the credential; the agent sees it redacted")
		Expect(call("fail", "x").IsError).To(BeTrue(), "the tool error path")
		Expect(call("echo", "refuse-me").IsError).To(BeTrue(), "the guard refusal path, whose reason quotes the value")
		gw.Drain()

		Expect(rec.Ended()).NotTo(BeEmpty(), "the calls wrote spans")
		Expect(audit.count()).To(Equal(3))
		Expect(logs.String()).To(ContainSubstring("mcp tool call"))
		expectNoLeak("a span", spanTexts(rec)...)
		expectNoLeak("the log", logs.String())
		expectNoLeak("a result to the agent", results...)
		expectNoLeak("an audit row", audit.verdicts()...)
		Expect(guard.sawText(fakeToken)).To(BeTrue(), "the guard still sees the raw message: it must, to judge it")
	})
})

// loggingAudit writes each row as koinon-gateway's logAudit does, and keeps the verdicts.
type loggingAudit struct {
	log  *slog.Logger
	mu   sync.Mutex
	rows []mcpgateway.AuditRow
}

func (a *loggingAudit) Record(ctx context.Context, r mcpgateway.AuditRow) error {
	a.mu.Lock()
	a.rows = append(a.rows, r)
	a.mu.Unlock()
	a.log.LogAttrs(ctx, slog.LevelInfo, "mcp tool call", slog.String("target", r.Target), slog.String("tool", r.Tool),
		slog.String("outcome", string(r.Outcome)), slog.String("verdict", r.Verdict), slog.String("credential", r.CredKind))
	return nil
}

func (a *loggingAudit) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.rows)
}

func (a *loggingAudit) verdicts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.rows))
	for _, r := range a.rows {
		out = append(out, r.Verdict)
	}
	return out
}
