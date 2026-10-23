// SPDX-License-Identifier: Apache-2.0

package fakemodel_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

func TestFakemodel(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "fakemodel")
}

// start serves the scripts in-process and closes the server after the spec.
func start(scripts ...fakemodel.Script) (*fakemodel.Server, *httptest.Server) {
	GinkgoHelper()
	srv, err := fakemodel.New(scripts...)
	Expect(err).NotTo(HaveOccurred())
	ts := httptest.NewServer(srv)
	DeferCleanup(ts.Close)
	return srv, ts
}

// post sends a JSON body and returns the response; the caller closes the body.
func post(ctx context.Context, url string, body any, header ...string) *http.Response {
	GinkgoHelper()
	b, err := json.Marshal(body)
	Expect(err).NotTo(HaveOccurred())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	return resp
}

// decode reads a whole JSON response body.
func decode(resp *http.Response, v any) {
	GinkgoHelper()
	DeferCleanup(resp.Body.Close)
	Expect(json.NewDecoder(resp.Body).Decode(v)).To(Succeed())
}

type event struct {
	name string
	data string
}

// readSSE parses a server-sent event stream as the SSE spec defines it (field lines, blank line
// dispatches). It returns the events read and the error that ended the stream, if any.
func readSSE(r io.Reader) ([]event, error) {
	var (
		events []event
		cur    event
		data   []string
	)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if data != nil {
				cur.data = strings.Join(data, "\n")
				events = append(events, cur)
			}
			cur, data = event{}, nil
		case strings.HasPrefix(line, "event:"):
			cur.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	return events, sc.Err()
}

func intPtr(i int) *int { return &i }
