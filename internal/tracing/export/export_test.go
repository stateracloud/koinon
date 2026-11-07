// SPDX-License-Identifier: Apache-2.0

package export_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/tracing/export"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

// Fake credentials, obviously fake and low-entropy.
const (
	fakeKey    = "koinon_test_not-a-real-token"
	fakePublic = "koinon_test_not-a-real-public"
)

// hangBound tells a hang from a slow machine. The client's own timeout is not what these specs test,
// and the 2 s it had was missed on a loaded external gate (koinon-private#612).
const hangBound = time.Minute

var _ = Describe("Client", func() {
	var (
		b      *backend
		client *export.Client
	)
	BeforeEach(func() {
		b = newBackend()
		DeferCleanup(b.Close)
		// The egress policy allows the test server by name; everything else private stays denied.
		host, _, err := net.SplitHostPort(b.Listener.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		policy := catalog.URLPolicy{Namespace: "koinon", AllowedHosts: []string{host}}
		client, err = export.NewClient(policy.DialContext(&net.Dialer{Timeout: hangBound}), hangBound)
		Expect(err).NotTo(HaveOccurred())
	})
	generic := func() export.Destination {
		return export.Destination{Preset: export.Generic, Endpoint: b.URL + "/v1/traces", Secrets: map[string]string{"X-Token": fakeKey}}
	}

	It("sends the synthetic trace as OTLP protobuf: a root span and one child, no content, koinon.test", func(ctx SpecContext) {
		out := client.Test(ctx, generic(), "org-1", map[string]string{"deployment.environment": "test"}, time.Now())
		Expect(out.OK).To(BeTrue(), out.Message)
		Expect(out.Status).To(Equal(200))

		req, hdr := b.last()
		Expect(hdr.Get("X-Token")).To(Equal(fakeKey))
		rs := req.GetResourceSpans()
		Expect(rs).To(HaveLen(1))
		Expect(rs[0].GetSchemaUrl()).To(Equal(semconv.SchemaURL))
		attrs := map[string]string{}
		for _, kv := range rs[0].GetResource().GetAttributes() {
			attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
		Expect(attrs).To(HaveKeyWithValue("service.name", "koinon"))
		Expect(attrs).To(HaveKeyWithValue("deployment.environment", "test"))

		spans := rs[0].GetScopeSpans()[0].GetSpans()
		Expect(spans).To(HaveLen(2))
		root, child := spans[0], spans[1]
		Expect(root.GetParentSpanId()).To(BeEmpty())
		Expect(child.GetParentSpanId()).To(Equal(root.GetSpanId()))
		Expect(child.GetTraceId()).To(Equal(root.GetTraceId()))
		Expect(root.GetName()).To(Equal("invoke_agent koinon-test"))
		Expect(child.GetName()).To(Equal("chat koinon-test"))
		for _, sp := range spans {
			got := map[string]bool{}
			for _, kv := range sp.GetAttributes() {
				switch kv.GetKey() {
				case semconv.KoinonTest:
					got[kv.GetKey()] = kv.GetValue().GetBoolValue()
				case semconv.KoinonOrg:
					Expect(kv.GetValue().GetStringValue()).To(Equal("org-1"))
				case semconv.GenAIOperationName, semconv.GenAIAgentName, semconv.GenAIRequestModel, semconv.GenAIProviderName:
				default:
					Fail("a test span carries an unexpected attribute: " + kv.GetKey())
				}
			}
			Expect(got).To(HaveKeyWithValue(semconv.KoinonTest, true))
		}
	})

	DescribeTable("each preset adds the headers its backend needs",
		func(ctx SpecContext, d export.Destination, want map[string]string) {
			d.Endpoint = b.URL + "/ingest"
			out := client.Test(ctx, d, "", nil, time.Now())
			Expect(out.OK).To(BeTrue(), out.Message)
			_, hdr := b.last()
			for k, v := range want {
				Expect(hdr.Get(k)).To(Equal(v), k)
			}
		},
		Entry("langfuse: Basic from the public and secret key", export.Destination{
			Preset: export.Langfuse, Secrets: map[string]string{"public_key": fakePublic, "secret_key": fakeKey},
		}, map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(fakePublic+":"+fakeKey))}),
		Entry("datadog: dd-api-key and the llmobs source", export.Destination{
			Preset: export.Datadog, Secrets: map[string]string{"api_key": fakeKey},
		}, map[string]string{"dd-api-key": fakeKey, "dd-otlp-source": "llmobs"}),
		Entry("langsmith: x-api-key and the project", export.Destination{
			Preset: export.LangSmith, Secrets: map[string]string{"api_key": fakeKey}, Params: map[string]string{"project": "koinon-dev"},
		}, map[string]string{"x-api-key": fakeKey, "Langsmith-Project": "koinon-dev"}),
		Entry("opik: the key as Authorization, the workspace and the project", export.Destination{
			Preset: export.Opik, Secrets: map[string]string{"api_key": fakeKey}, Params: map[string]string{"workspace": "acme", "project": "agents"},
		}, map[string]string{"Authorization": fakeKey, "Comet-Workspace": "acme", "projectName": "agents"}),
		Entry("generic: the headers as given", export.Destination{
			Preset: export.Generic, Secrets: map[string]string{"X-Scope-OrgID": "acme", "Authorization": "Bearer " + fakeKey},
		}, map[string]string{"X-Scope-OrgID": "acme", "Authorization": "Bearer " + fakeKey}),
	)

	It("saves nothing it should not: a 2xx with rejected spans is not OK", func(ctx SpecContext) {
		b.reply(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-protobuf")
			out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{PartialSuccess: &coltracepb.ExportTracePartialSuccess{
				RejectedSpans: 1, ErrorMessage: "span rejected for key " + fakeKey,
			}})
			_, _ = w.Write(out)
		})
		res, err := client.Export(ctx, generic(), &coltracepb.ExportTraceServiceRequest{})
		Expect(err).NotTo(HaveOccurred(), "a partial success is a result, the router counts the rejected spans")
		Expect(res.Rejected).To(Equal(int64(1)))

		out := client.Test(ctx, generic(), "", nil, time.Now())
		Expect(out.OK).To(BeFalse())
		Expect(out.Rejected).To(Equal(int64(1)))
		Expect(out.Message).To(ContainSubstring("rejected 1 span"))
		Expect(out.Message).NotTo(ContainSubstring("span rejected"), "what the backend wrote is never quoted")
	})

	It("never quotes a failure body, even one that echoes the credentials in another form", func(ctx SpecContext) {
		b.reply(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("bad auth " + base64.StdEncoding.EncodeToString([]byte(fakePublic+":"+fakeKey))))
		})
		d := export.Destination{Preset: export.Langfuse, Endpoint: b.URL, Secrets: map[string]string{"public_key": fakePublic, "secret_key": fakeKey}}
		out := client.Test(ctx, d, "", nil, time.Now())
		Expect(out.OK).To(BeFalse())
		Expect(out.Message).To(Equal("the backend answered HTTP 401 (the credentials were refused)"))
	})

	It("treats a 2xx without a protobuf body as accepted", func(ctx SpecContext) {
		b.reply(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})
		Expect(client.Test(ctx, generic(), "", nil, time.Now()).OK).To(BeTrue())
	})

	DescribeTable("a refusal is an error with a kind, and never carries the credential",
		func(ctx SpecContext, status int, kind export.Kind, retryable bool) {
			b.reply(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("denied: " + fakeKey))
			})
			_, err := client.Export(ctx, generic(), &coltracepb.ExportTraceServiceRequest{})
			var ee *export.Error
			Expect(errors.As(err, &ee)).To(BeTrue())
			Expect(ee.Kind).To(Equal(kind))
			Expect(ee.Status).To(Equal(status))
			Expect(ee.Retryable()).To(Equal(retryable))
			Expect(ee.Message).NotTo(ContainSubstring(fakeKey))
			if kind == export.KindServer {
				Expect(ee.RetryAfter).To(Equal(7 * time.Second))
			}
			out := client.Test(ctx, generic(), "", nil, time.Now())
			Expect(out.OK).To(BeFalse())
			Expect(out.Status).To(Equal(status))
		},
		Entry("401", http.StatusUnauthorized, export.KindRejected, false),
		Entry("400", http.StatusBadRequest, export.KindRejected, false),
		Entry("429", http.StatusTooManyRequests, export.KindServer, true),
		Entry("503", http.StatusServiceUnavailable, export.KindServer, true),
	)

	It("follows no redirect", func(ctx SpecContext) {
		target := newBackend()
		DeferCleanup(target.Close)
		b.reply(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		})
		_, err := client.Export(ctx, generic(), &coltracepb.ExportTraceServiceRequest{})
		var ee *export.Error
		Expect(errors.As(err, &ee)).To(BeTrue())
		Expect(ee.Kind).To(Equal(export.KindRedirect))
		Expect(ee.Retryable()).To(BeFalse())
		Expect(target.count()).To(BeZero(), "the redirect target must not be called, with or without the credentials")
		Expect(client.Test(ctx, generic(), "", nil, time.Now()).OK).To(BeFalse())
	})

	It("dials only what the egress policy allows", func(ctx SpecContext) {
		// A policy that does not list the test server's host denies the loopback address.
		strict, err := export.NewClient(catalog.URLPolicy{Namespace: "koinon"}.DialContext(&net.Dialer{Timeout: time.Second}), time.Second)
		Expect(err).NotTo(HaveOccurred())
		out := strict.Test(ctx, generic(), "", nil, time.Now())
		Expect(out.OK).To(BeFalse())
		Expect(out.Message).To(ContainSubstring("egress policy"))
		Expect(b.count()).To(BeZero())

		_, err = export.NewClient(nil, 0)
		Expect(err).To(HaveOccurred(), "a client without a policy is refused")
	})

	It("reports an unreachable endpoint without quoting the address", func(ctx SpecContext) {
		b.Close()
		out := client.Test(ctx, generic(), "", nil, time.Now())
		Expect(out.OK).To(BeFalse())
		Expect(out.Message).To(HavePrefix("could not send"))
		Expect(out.Message).NotTo(ContainSubstring("127.0.0.1"))
	})

	It("honours the caller's context", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.Export(ctx, generic(), &coltracepb.ExportTraceServiceRequest{})
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Destination", func() {
	DescribeTable("validation refuses",
		func(d export.Destination, substr string) {
			err := d.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(substr))
			Expect(err.Error()).NotTo(ContainSubstring(fakeKey))
		},
		Entry("an unknown backend", export.Destination{Preset: "honeycomb", Endpoint: "https://a.example.com"}, "backend must be one of"),
		Entry("a non-http endpoint", export.Destination{Preset: export.Datadog, Endpoint: "ftp://a.example.com", Secrets: map[string]string{"api_key": fakeKey}}, "http(s)"),
		Entry("credentials in the URL", export.Destination{Preset: export.Datadog, Endpoint: "https://u:p@a.example.com", Secrets: map[string]string{"api_key": fakeKey}}, "must not carry credentials"),
		Entry("a missing key", export.Destination{Preset: export.Langfuse, Endpoint: "https://a.example.com", Secrets: map[string]string{"public_key": fakePublic}}, "needs secret_key"),
		Entry("a setting the preset lacks", export.Destination{Preset: export.Datadog, Endpoint: "https://a.example.com", Secrets: map[string]string{"api_key": fakeKey}, Params: map[string]string{"x": "y"}}, "no setting"),
		Entry("a secret the preset lacks", export.Destination{Preset: export.Datadog, Endpoint: "https://a.example.com", Secrets: map[string]string{"api_key": fakeKey, "other": "z"}}, "no secret"),
		Entry("a generic destination without headers", export.Destination{Preset: export.Generic, Endpoint: "https://a.example.com"}, "at least one header"),
		Entry("a bad header name", export.Destination{Preset: export.Generic, Endpoint: "https://a.example.com", Secrets: map[string]string{"bad name": fakeKey}}, "not a valid header name"),
		Entry("a header Koinon sets", export.Destination{Preset: export.Generic, Endpoint: "https://a.example.com", Secrets: map[string]string{"Content-Type": "x"}}, "set by Koinon"),
		Entry("a header value with a newline", export.Destination{Preset: export.Generic, Endpoint: "https://a.example.com", Secrets: map[string]string{"X-A": "a\r\nX-B: b"}}, "control characters"),
	)

	It("lets a self-hosted Opik run without authentication", func() {
		d := export.Destination{Preset: export.Opik, Endpoint: "http://opik.lab.example.com/api/v1/private/otel/v1/traces"}
		h, err := d.Headers()
		Expect(err).NotTo(HaveOccurred())
		Expect(h).To(BeEmpty())
	})

	It("normalises an endpoint and compares origins by scheme, host and port", func() {
		n, err := export.NormalizeEndpoint("HTTPS://Cloud.Example.COM/Path?x=1")
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal("https://cloud.example.com/Path?x=1"))
		Expect(export.SameOrigin("https://a.example.com/x", "https://A.example.com:443/y")).To(BeTrue())
		Expect(export.SameOrigin("https://a.example.com", "http://a.example.com")).To(BeFalse())
		Expect(export.SameOrigin("https://a.example.com", "https://a.example.com:8443")).To(BeFalse())
		Expect(export.SameOrigin("https://a.example.com", "https://evil.example.com")).To(BeFalse())
	})

	It("hides its secrets when printed", func() {
		d := export.Destination{Preset: export.Datadog, Endpoint: "https://a.example.com/v1/traces?token=" + fakeKey, Secrets: map[string]string{"api_key": fakeKey}}
		for _, s := range []string{d.String(), d.GoString()} {
			Expect(s).NotTo(ContainSubstring(fakeKey))
		}
	})
})
