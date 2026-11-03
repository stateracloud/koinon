// SPDX-License-Identifier: Apache-2.0

package awskms_test

import (
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore/awskms"
)

// The signer against AWS's published Signature Version 4 test suite (aws-sig-v4-test-suite, the
// vectors the AWS SDKs test with): the example credentials from AWS's documentation, region
// us-east-1, service "service", 2015-08-30 12:36:00 UTC. No real credential.
var _ = Describe("Signature Version 4", func() {
	const (
		exampleAccessKey = "AKIDEXAMPLE"
		exampleSecret    = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY" // AWS's documented example key
	)
	at := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

	DescribeTable("signs the test suite's requests as AWS does",
		func(method, target string, headers [][2]string, body, signedHeaders, signature string) {
			req, err := http.NewRequest(method, "https://example.amazonaws.com"+target, strings.NewReader(body))
			Expect(err).NotTo(HaveOccurred())
			for _, h := range headers {
				req.Header.Add(h[0], h[1])
			}
			awskms.Sign(req, []byte(body), exampleAccessKey, exampleSecret, "", "us-east-1", "service", at)
			Expect(req.Header.Get("X-Amz-Date")).To(Equal("20150830T123600Z"))
			Expect(req.Header.Get("Authorization")).To(Equal(
				"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=" +
					signedHeaders + ", Signature=" + signature))
		},
		Entry("get-vanilla", "GET", "/", nil, "", "host;x-amz-date",
			"5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"),
		Entry("post-vanilla", "POST", "/", nil, "", "host;x-amz-date",
			"5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b"),
		Entry("get-vanilla-query-order-key-case", "GET", "/?Param2=value2&Param1=value1", nil, "", "host;x-amz-date",
			"b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500"),
		Entry("post-x-www-form-urlencoded", "POST", "/",
			[][2]string{{"Content-Type", "application/x-www-form-urlencoded"}}, "Param1=value1",
			"content-type;host;x-amz-date",
			"ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a"),
		Entry("get-header-value-trim", "GET", "/",
			[][2]string{{"My-Header1", " value1"}, {"My-Header2", ` "a   b   c"`}}, "",
			"host;my-header1;my-header2;x-amz-date",
			"acc3ed3afb60bb290fc8d2dd0098b9911fcaa05412b367055dee359757a9c736"),
	)

	It("derives the signing key as AWS's documented example does", func() {
		// "Examples of how to derive a signing key for Signature Version 4" (IAM user guide).
		k := awskms.SigningKey(exampleSecret, "20120215", "us-east-1", "iam")
		Expect(hex.EncodeToString(k)).To(Equal("f4780e2d9f65fa895f9c67b32ce1baf0b0d8a43505a000a1a9e090d414db404d"))
	})

	It("signs the session token of temporary credentials", func() {
		req, err := http.NewRequest("POST", "https://kms.eu-central-1.amazonaws.com/", strings.NewReader("{}"))
		Expect(err).NotTo(HaveOccurred())
		awskms.Sign(req, []byte("{}"), "koinon_test_not-a-real-key-id", "koinon_test_not-a-real-secret",
			"koinon_test_not-a-real-session-token", "eu-central-1", "kms", at)
		Expect(req.Header.Get("X-Amz-Security-Token")).To(Equal("koinon_test_not-a-real-session-token"))
		Expect(req.Header.Get("Authorization")).To(ContainSubstring("SignedHeaders=host;x-amz-date;x-amz-security-token,"))
		Expect(req.Header.Get("Authorization")).NotTo(ContainSubstring("koinon_test_not-a-real-secret"))
	})
})
