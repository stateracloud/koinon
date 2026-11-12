// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"encoding/json"
	"os"
	"slices"

	g "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/sinkclass"
)

// The tool declarations the template legs test reads (templates/legs_test.go). The guard must give
// each of them the same result legs as before resultLegs called sinkclass.Declaration.ResultLegs (#129).
const templateToolDeclarations = "../../templates/testdata/legs/tool-declarations.json"

// resultLegsBefore is resultLegs as it was before #129, kept here as the reference the refactor is
// compared with: the server's legs, each one replaced by the tool's own declaration where it has one.
func resultLegsBefore(srv store.McpServer, d sinkclass.Declaration) Legs {
	l := Legs{UntrustedInput: srv.ExposesUntrustedInput, PrivateData: srv.ExposesPrivateData}
	if d.ResultUntrustedInput != nil {
		l.UntrustedInput = *d.ResultUntrustedInput
	}
	if d.ResultPrivateData != nil {
		l.PrivateData = *d.ResultPrivateData
	}
	return l
}

var _ = g.Describe("resultLegs (ADR 0091 decision 4)", func() {
	servers := []store.McpServer{
		{},
		{ExposesUntrustedInput: true},
		{ExposesPrivateData: true},
		{ExposesUntrustedInput: true, ExposesPrivateData: true},
	}

	// declarations are every tool of templateToolDeclarations, an unknown tool (the guard gives it
	// sinkclass.Default, guard.go) and every combination of unset, true and false result legs.
	declarations := func() map[string]sinkclass.Declaration {
		raw, err := os.ReadFile(templateToolDeclarations)
		Expect(err).NotTo(HaveOccurred())
		var decls map[string]sinkclass.Declaration
		Expect(json.Unmarshal(raw, &decls)).To(Succeed())
		Expect(decls).NotTo(BeEmpty())
		decls["unknown tool"] = sinkclass.Default()
		t, f := true, false
		for i, u := range []*bool{nil, &t, &f} {
			for j, p := range []*bool{nil, &t, &f} {
				d := sinkclass.Default()
				d.ResultUntrustedInput, d.ResultPrivateData = u, p
				decls["combination "+string(rune('0'+i))+string(rune('0'+j))] = d
			}
		}
		return decls
	}

	g.It("gives every declared tool and an unknown one the same legs as before, for every server", func() {
		decls := declarations()
		names := make([]string, 0, len(decls))
		for name := range decls {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			for _, srv := range servers {
				Expect(resultLegs(srv, decls[name])).To(Equal(resultLegsBefore(srv, decls[name])),
					"tool %q, server legs untrusted=%t private=%t", name, srv.ExposesUntrustedInput, srv.ExposesPrivateData)
			}
		}
	})

	g.It("gives an unknown tool on a server nobody classified both legs (fail closed)", func() {
		// A server nobody classified carries both legs (mcpservers.DefaultDeclaration).
		srv := store.McpServer{ExposesUntrustedInput: true, ExposesPrivateData: true}
		Expect(sinkclass.Default().Class).To(Equal(sinkclass.ExternalHigh))
		Expect(resultLegs(srv, sinkclass.Default())).To(Equal(Legs{UntrustedInput: true, PrivateData: true}))
	})
})
