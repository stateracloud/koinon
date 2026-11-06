// SPDX-License-Identifier: Apache-2.0

package checkpoint_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/checkpoint"
)

// agentd asks koinon-api for each repository password; it holds no KEK (ADR 0054).
var _ = Describe("Repository passwords over HTTP", func() {
	key := []byte("koinon_test_not-a-real-key")
	org := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	agent := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	now := time.Now()

	It("accepts a token for its agent and expiry only", func() {
		exp := now.Add(20 * time.Second).Unix()
		tok := checkpoint.RepositoryToken(key, org.String(), agent.String(), exp)
		Expect(checkpoint.ValidRepositoryToken(key, tok, org.String(), agent.String(), exp, now)).To(BeTrue())
		Expect(checkpoint.ValidRepositoryToken(key, tok, org.String(), pgtype.UUID{Bytes: [16]byte{3}, Valid: true}.String(), exp, now)).To(BeFalse())
		Expect(checkpoint.ValidRepositoryToken([]byte("other"), tok, org.String(), agent.String(), exp, now)).To(BeFalse())
		Expect(checkpoint.ValidRepositoryToken(key, tok, org.String(), agent.String(), exp, now.Add(time.Minute))).To(BeFalse(), "expired")
		far := now.Add(time.Hour).Unix()
		Expect(checkpoint.ValidRepositoryToken(key, checkpoint.RepositoryToken(key, org.String(), agent.String(), far), org.String(), agent.String(), far, now)).To(BeFalse(), "too far ahead")
	})

	It("the Server refuses a request without a valid credential before it opens anything", func() {
		srv := httptest.NewServer(checkpoint.Server{Key: func(context.Context) ([]byte, error) { return key, nil }})
		DeferCleanup(srv.Close)
		_, err := checkpoint.Remote{BaseURL: srv.URL, Key: func() ([]byte, error) { return []byte("wrong"), nil }}.Get(context.Background(), org, agent)
		Expect(err).To(MatchError(ContainSubstring("401")))

		res, err := http.Get(srv.URL + checkpoint.RepositoryPath)
		Expect(err).NotTo(HaveOccurred())
		_ = res.Body.Close()
		Expect(res.StatusCode).To(Equal(http.StatusMethodNotAllowed))
	})

	It("the Remote signs its request and reads the reply", func() {
		stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			exp, _ := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
			tok := r.Header.Get("Authorization")[len("Bearer "):]
			if r.Method != http.MethodPost || !checkpoint.ValidRepositoryToken(key, tok, r.URL.Query().Get("org"), r.URL.Query().Get("agent"), exp, time.Now()) {
				http.Error(w, "bad credential", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"prefix":"orgs/o/homes/a/","password":"koinon_test_not-a-real-token"}`))
		}))
		DeferCleanup(stub.Close)
		got, err := checkpoint.Remote{BaseURL: stub.URL, Key: func() ([]byte, error) { return key, nil }}.Get(context.Background(), org, agent)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(checkpoint.Repository{Prefix: "orgs/o/homes/a/", Password: "koinon_test_not-a-real-token"}))
	})

	It("asks for an epoch only when one is wanted, and refuses an answer from another epoch", func() {
		var asked []string
		stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A koinon-api older than repository epochs: no epoch in the reply, whatever is asked.
			asked = append(asked, r.URL.Query().Get("epoch")+"|"+strconv.FormatBool(r.URL.Query().Has("epoch")))
			_, _ = w.Write([]byte(`{"prefix":"orgs/o/homes/a/","password":"koinon_test_not-a-real-token"}`))
		}))
		DeferCleanup(stub.Close)
		remote := checkpoint.Remote{BaseURL: stub.URL, Key: func() ([]byte, error) { return key, nil }}
		_, err := remote.Get(context.Background(), org, agent)
		Expect(err).NotTo(HaveOccurred())
		zero, err := remote.At(context.Background(), org, agent, 0)
		Expect(err).NotTo(HaveOccurred(), "epoch 0 is the older koinon-api's only repository")
		Expect(zero.Epoch).To(BeZero())
		_, err = remote.At(context.Background(), org, agent, 1)
		Expect(err).To(MatchError(ContainSubstring("not 1")))
		Expect(asked).To(Equal([]string{"|false", "0|true", "1|true"}))
	})

	It("the Server refuses a malformed epoch", func() {
		srv := httptest.NewServer(checkpoint.Server{Key: func(context.Context) ([]byte, error) { return key, nil }})
		DeferCleanup(srv.Close)
		for _, e := range []string{"-1", "x", "99999999999"} {
			exp := strconv.FormatInt(time.Now().Add(20*time.Second).Unix(), 10)
			res, err := http.Post(srv.URL+checkpoint.RepositoryPath+"?org="+org.String()+"&agent="+agent.String()+"&exp="+exp+"&epoch="+e, "", nil)
			Expect(err).NotTo(HaveOccurred())
			_ = res.Body.Close()
			Expect(res.StatusCode).To(Equal(http.StatusBadRequest), e)
		}
	})
})
