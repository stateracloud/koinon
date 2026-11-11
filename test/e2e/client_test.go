// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"

	. "github.com/onsi/gomega"
)

// fakeGitHub reads the fake GitHub's test-only endpoints (cmd/koinon-fakegithub).
type fakeGitHub struct{ base string }

type fakeApp struct {
	AppID          int64  `json:"appId"`
	InstallationID int64  `json:"installationId"`
	Account        string `json:"account"`
	PrivateKey     string `json:"privateKey"`
}

type fakePull struct {
	Number  int64
	Title   string
	Body    string
	Head    string
	Base    string
	HeadSHA string
	HTMLURL string
}

type fakeRepo struct {
	Refs  map[string]string            `json:"refs"`
	Pulls []fakePull                   `json:"pulls"`
	Calls map[string]int               `json:"calls"`
	Files map[string]map[string]string `json:"files"`
	Mints int                          `json:"mints"`
}

func (f fakeGitHub) get(ctx context.Context, path string, out any) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.base+path, nil)
	Expect(err).NotTo(HaveOccurred())
	res, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = res.Body.Close() }()
	Expect(res.StatusCode).To(Equal(http.StatusOK))
	Expect(json.NewDecoder(res.Body).Decode(out)).To(Succeed())
}

func (f fakeGitHub) app(ctx context.Context) fakeApp {
	var a fakeApp
	f.get(ctx, "/_fake/app", &a)
	return a
}

func (f fakeGitHub) repo(ctx context.Context, name string) fakeRepo {
	var r fakeRepo
	f.get(ctx, "/_fake/repos/"+name, &r)
	return r
}
