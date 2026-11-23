// SPDX-License-Identifier: Apache-2.0

// Package mcpserverstest holds test helpers for package mcpservers.
package mcpserverstest

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above

	"github.com/stateracloud/koinon/internal/mcpservers"
)

// PinOf returns the tool text hash an admin reads before an approval (Tool.ToolHash), as the console
// does. A spec passes it to SetTier or AcknowledgeReview (#1173). It is empty for a tool the server
// does not have.
func PinOf(ctx context.Context, svc *mcpservers.Service, org pgtype.UUID, serverID, tool string) string {
	GinkgoHelper()
	tools, err := svc.Tools(ctx, org, serverID)
	Expect(err).NotTo(HaveOccurred())
	for _, t := range tools {
		if t.Name == tool {
			return t.ToolHash
		}
	}
	return ""
}
