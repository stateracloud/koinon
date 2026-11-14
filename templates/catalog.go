// SPDX-License-Identifier: Apache-2.0

package templates

import (
	"errors"
	"fmt"
	"slices"
)

// Catalog is what a template names but this package cannot see: the connector types and their
// scopes, and the tool names the platform knows. The connectors package imports this one, so the
// caller builds the Catalog from it (see CheckEmbedded).
type Catalog struct {
	// Scopes maps a connector type ID to the IDs of its scopes.
	Scopes map[string][]string
	// KnownTool reports whether a tool name is one the platform knows: a platform tool, a tool of a
	// connector scope, or a tool with a declaration (ADR 0091).
	KnownTool func(name string) bool
}

// CheckCatalog checks the names a template uses against the catalog: every grant names a connector
// type that exists and scopes that exist on it, and every spec.tools entry is a known tool.
func (t *Template) CheckCatalog(c Catalog) error {
	for i, g := range t.Spec.Grants {
		scopes, ok := c.Scopes[g.Connector]
		if !ok {
			return fmt.Errorf("grants[%d]: unknown connector %q", i, g.Connector)
		}
		for _, s := range g.Scopes {
			if !slices.Contains(scopes, s) {
				return fmt.Errorf("grants[%d]: connector %q has no scope %q", i, g.Connector, s)
			}
		}
	}
	for _, name := range t.Spec.Tools {
		if c.KnownTool == nil || !c.KnownTool(name) {
			return fmt.Errorf("spec.tools: unknown tool %q", name)
		}
	}
	return nil
}

// CheckShipped holds a template that ships in the binary to the bounds a user-written template may
// leave to the platform defaults: it sets both run bounds, its run spend stays within its spend
// limit, and every grant says how many targets the user picks.
func (t *Template) CheckShipped() error {
	run := t.Spec.Limits.Run
	if run.MaxModelRequests == 0 {
		return errors.New("limits.run.maxModelRequests is missing: a shipped template sets it")
	}
	if run.MaxSpendUSD == 0 {
		return errors.New("limits.run.maxSpendUSD is missing: a shipped template sets it")
	}
	if run.MaxSpendUSD > t.Spec.Limits.SpendUSD {
		return fmt.Errorf("limits.run.maxSpendUSD %s is greater than limits.spendUSD %s", run.MaxSpendUSD, t.Spec.Limits.SpendUSD)
	}
	for i, g := range t.Spec.Grants {
		if g.Targets == nil {
			return fmt.Errorf("grants[%d].targets is missing: a shipped template bounds the targets", i)
		}
	}
	return nil
}

// CheckEmbedded runs the rules of Parse and CheckShipped (both already done by All) and CheckCatalog
// over every embedded template and joins the errors, each naming its template.
//
// The connectors package imports templates, so the catalog check of the embedded templates runs in
// rules_test.go with connectors.All(). The embedded content is fixed at build time, so that test
// checks what ships.
func CheckEmbedded(c Catalog) error {
	ts, err := All()
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range ts {
		if err := t.CheckShipped(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Metadata.ID, err))
		}
		if err := t.CheckCatalog(c); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Metadata.ID, err))
		}
	}
	return errors.Join(errs...)
}
