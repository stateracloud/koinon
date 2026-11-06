// SPDX-License-Identifier: Apache-2.0

package agentgateway

import (
	"fmt"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
)

// FormatLogExpr is the CEL expression of the access-log field LogFieldFormat: the format whose route
// the request used, from the paths of the gateway's rendering table.
func FormatLogExpr() string {
	expr := `""`
	for _, f := range modelformat.All() {
		if r, err := gateway.RenderingOf(gateway.Model{Format: f}); err == nil {
			expr = fmt.Sprintf(`request.path == %q ? %q : %s`, r.Path, string(f), expr)
		}
	}
	return expr
}
