// SPDX-License-Identifier: Apache-2.0

package substrateinstall_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/substrateinstall"
)

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%[1]s</Message></Error>`, code)
}

// The real S3 client must tell a missing object (stage it) from every other error (stop).
func TestS3AssetStoreDigest(t *testing.T) {
	g := NewWithT(t)
	body := []byte("koinon_test_not-a-real-tarball")
	sum := sha256.Sum256(body)
	cases := []struct {
		name    string
		handler http.HandlerFunc
		found   bool
		sum     string
		errPart string
	}{
		{"match", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
			w.Header().Set("ETag", `"x"`)
			_, _ = w.Write(body)
		}, true, hex.EncodeToString(sum[:]), ""},
		{"missing object", func(w http.ResponseWriter, _ *http.Request) { s3Error(w, 404, "NoSuchKey") }, false, "", ""},
		{"missing bucket", func(w http.ResponseWriter, _ *http.Request) { s3Error(w, 404, "NoSuchBucket") }, false, "", "does not exist"},
		{"access denied", func(w http.ResponseWriter, _ *http.Request) { s3Error(w, 403, "AccessDenied") }, false, "", "AccessDenied"},
		{"server error", func(w http.ResponseWriter, _ *http.Request) { s3Error(w, 500, "InternalError") }, false, "", "InternalError"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(c.handler)
		store, err := substrateinstall.NewS3AssetStore(srv.URL, "ate-snapshots", "koinon_test_access", "koinon_test_secret")
		g.Expect(err).NotTo(HaveOccurred(), c.name)
		got, found, err := store.Digest(context.Background(), "gvisor-assets/amd64/x")
		srv.Close()
		if c.errPart != "" {
			g.Expect(err).To(MatchError(ContainSubstring(c.errPart)), c.name)
			continue
		}
		g.Expect(err).NotTo(HaveOccurred(), c.name)
		g.Expect(found).To(Equal(c.found), c.name)
		g.Expect(got).To(Equal(c.sum), c.name)
	}
}
