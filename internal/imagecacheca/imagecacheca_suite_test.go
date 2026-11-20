// SPDX-License-Identifier: Apache-2.0

package imagecacheca_test

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestImageCacheCA(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Image Cache CA Suite")
}

// TestRootsChild is run by the SSL_CERT_DIR spec in a child process (Go reads the system roots once, from the
// environment of the process). Without KOINON_IMAGECACHECA_CHILD it does nothing.
func TestRootsChild(t *testing.T) {
	caFile := os.Getenv("KOINON_IMAGECACHECA_CHILD")
	if caFile == "" {
		t.Skip("only the SSL_CERT_DIR spec starts this test")
	}
	trusted := func(pemData []byte) bool {
		block, _ := pem.Decode(pemData)
		if block == nil {
			return false
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return false
		}
		_, err = cert.Verify(x509.VerifyOptions{CurrentTime: time.Now()})
		return err == nil
	}
	raw, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("CA trusted=%v\n", trusted(raw))
	// The first certificate of the default file, when this machine has one.
	if def, err := os.ReadFile("/etc/ssl/certs/ca-certificates.crt"); err == nil {
		fmt.Printf("default roots kept=%v\n", trusted(def))
	} else {
		fmt.Println("default roots kept=n/a")
	}
}
