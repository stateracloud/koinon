// SPDX-License-Identifier: Apache-2.0

package gitea_test

import "encoding/pem"

func pemEncode(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
