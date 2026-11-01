// SPDX-License-Identifier: Apache-2.0

package koinonagentd

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// noDB is a database that is not there: the schema gate never turns ready, which these specs do
// not need.
type noDB struct{}

func (noDB) QueryRow(context.Context, string, ...any) pgx.Row { return errRow{} }

type errRow struct{}

func (errRow) Scan(...any) error { return context.DeadlineExceeded }

var _ = Describe("Health listener", func() {
	get := func(addr, path string) int {
		req, err := http.NewRequestWithContext(GinkgoT().Context(), http.MethodGet, "http://"+addr+path, nil)
		Expect(err).NotTo(HaveOccurred())
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	It("keeps answering /healthz while draining after SIGTERM, and stops when the drain ends", func() {
		life, stopLife := context.WithCancel(context.Background())
		defer stopLife()
		term, sigterm := context.WithCancel(context.Background())
		defer sigterm()
		addr, err := serveHealth(life, term, "127.0.0.1:0", noDB{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		Expect(err).NotTo(HaveOccurred())
		Expect(get(addr, "/healthz")).To(Equal(http.StatusOK))

		sigterm() // SIGTERM: the drain starts
		Expect(get(addr, "/healthz")).To(Equal(http.StatusOK), "liveness must hold while agentd drains")
		Expect(get(addr, "/readyz")).To(Equal(http.StatusServiceUnavailable))

		stopLife() // the drain is over
		Eventually(func() error {
			req, _ := http.NewRequestWithContext(GinkgoT().Context(), http.MethodGet, "http://"+addr+"/healthz", nil)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			return err
		}).Should(HaveOccurred())
	})
})
