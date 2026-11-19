// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"errors"

	"context"
	"encoding/xml"
	"github.com/minio/minio-go/v7"
	"github.com/stateracloud/koinon/internal/execution"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution/substrate"
)

// fakeS3 serves the two calls of the reader: a v2 listing by prefix and a GET of one object.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]string
}

func (f *fakeS3) set(k, v string) { f.mu.Lock(); f.objects[k] = v; f.mu.Unlock() }
func (f *fakeS3) del(k string)    { f.mu.Lock(); delete(f.objects, k); f.mu.Unlock() }

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket != "koinon-test" {
		http.Error(w, "no such bucket", http.StatusNotFound)
		return
	}
	if key == "" {
		type content struct {
			Key  string
			Size int
		}
		var res struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Name     string
			Prefix   string
			KeyCount int
			Contents []content
		}
		res.Name, res.Prefix = bucket, r.URL.Query().Get("prefix")
		for k, v := range f.objects {
			if strings.HasPrefix(k, res.Prefix) {
				res.Contents = append(res.Contents, content{k, len(v)})
			}
		}
		sort.Slice(res.Contents, func(i, j int) bool { return res.Contents[i].Key < res.Contents[j].Key })
		res.KeyCount = len(res.Contents)
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(res)
		return
	}
	v, ok := f.objects[key]
	if !ok {
		http.Error(w, `<Error><Code>NoSuchKey</Code></Error>`, http.StatusNotFound)
		return
	}
	w.Header().Set("Last-Modified", "Thu, 19 Nov 2026 12:00:00 GMT")
	w.Header().Set("ETag", `"fake"`)
	_, _ = w.Write([]byte(v))
}

var _ = Describe("ObjectStore.ContentDigester", func() {
	var (
		s3     *fakeS3
		digest func(context.Context, string) (string, error)
	)
	BeforeEach(func() {
		s3 = &fakeS3{objects: map[string]string{
			"snap-1/mem.img": "memory", "snap-1/disk/a": "disk-a", "snap-10/mem.img": "another snapshot",
		}}
		srv := httptest.NewServer(s3)
		DeferCleanup(srv.Close)
		var err error
		digest, err = substrate.ObjectStore{
			Endpoint: srv.URL, Region: "us-east-1", Bucket: "koinon-test",
			AccessKeyID: "koinon_test_not-a-real-key", SecretAccessKey: "koinon_test_not-a-real-secret",
		}.ContentDigester()
		Expect(err).NotTo(HaveOccurred())
	})

	It("is stable, and covers every object under the prefix and no other", func(ctx SpecContext) {
		d1, err := digest(ctx, "s3://koinon-test/snap-1")
		Expect(err).NotTo(HaveOccurred())
		d2, err := digest(ctx, "s3://koinon-test/snap-1/")
		Expect(err).NotTo(HaveOccurred())
		Expect(d2).To(Equal(d1))
		Expect(d1).To(MatchRegexp(`^[0-9a-f]{64}$`))
		s3.set("snap-10/mem.img", "changed") // snap-10 is not under snap-1/
		d3, err := digest(ctx, "s3://koinon-test/snap-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(d3).To(Equal(d1))
	})
	It("changes when an object changes, appears or disappears, with the same size too", func(ctx SpecContext) {
		base, err := digest(ctx, "s3://koinon-test/snap-1")
		Expect(err).NotTo(HaveOccurred())
		seen := map[string]bool{base: true}
		for _, change := range []func(){
			func() { s3.set("snap-1/mem.img", "memorY") }, // same size, other bytes
			func() { s3.set("snap-1/extra", "x") },
			func() { s3.del("snap-1/disk/a") },
		} {
			change()
			d, err := digest(ctx, "s3://koinon-test/snap-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(seen).NotTo(HaveKey(d))
			seen[d] = true
		}
	})
	It("fails on a missing snapshot, another bucket, a bare bucket and a bad address", func(ctx SpecContext) {
		for uri, want := range map[string]string{
			"s3://koinon-test/nope":    "holds no object",
			"s3://other-bucket/snap-1": "not the configured snapshot bucket",
			"s3://koinon-test/":        "never covers a whole bucket",
			"not a uri":                "is not <scheme>://<bucket>/<prefix>",
		} {
			_, err := digest(ctx, uri)
			Expect(err).To(MatchError(ContainSubstring(want)), uri)
		}
	})
	It("refuses a store without endpoint or bucket", func() {
		_, err := substrate.ObjectStore{Endpoint: "x:1"}.ContentDigester()
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("storeError", func() {
	It("gives a gone snapshot the kind ErrNotFound", func() {
		err := substrate.StoreError("RestoreSandbox", errors.New("x"))
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(substrate.StoreError("RestoreSandbox", substrate.ErrNoObject), execution.ErrNotFound)).To(BeTrue())
		Expect(errors.Is(substrate.StoreError("RestoreSandbox", minio.ErrorResponse{Code: "AccessDenied"}), execution.ErrPermissionDenied)).To(BeTrue())
		Expect(errors.Is(substrate.StoreError("RestoreSandbox", minio.ErrorResponse{Code: "SlowDown"}), execution.ErrUnavailable)).To(BeTrue())
	})
})
