// SPDX-License-Identifier: Apache-2.0

package export_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"

	. "github.com/onsi/gomega"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// backend is a fake OTLP/HTTP backend: it records what it was sent and answers as told.
type backend struct {
	*httptest.Server
	mu       sync.Mutex
	headers  []http.Header
	requests []*coltracepb.ExportTraceServiceRequest
	// answer decides the reply; the default is 200 with an empty response.
	answer func(w http.ResponseWriter, r *http.Request)
}

func newBackend() *backend {
	b := &backend{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req coltracepb.ExportTraceServiceRequest
		Expect(proto.Unmarshal(body, &req)).To(Succeed())
		b.mu.Lock()
		b.headers = append(b.headers, r.Header.Clone())
		b.requests = append(b.requests, &req)
		answer := b.answer
		b.mu.Unlock()
		Expect(r.Method).To(Equal(http.MethodPost))
		Expect(r.Header.Get("Content-Type")).To(Equal("application/x-protobuf"))
		if answer != nil {
			answer(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
		_, _ = w.Write(out)
	}))
	return b
}

func (b *backend) reply(f func(w http.ResponseWriter, r *http.Request)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.answer = f
}

func (b *backend) last() (*coltracepb.ExportTraceServiceRequest, http.Header) {
	b.mu.Lock()
	defer b.mu.Unlock()
	Expect(b.requests).NotTo(BeEmpty())
	return b.requests[len(b.requests)-1], b.headers[len(b.headers)-1]
}

func (b *backend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.requests)
}
