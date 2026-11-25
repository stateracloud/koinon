// SPDX-License-Identifier: Apache-2.0

// Command vapcheck compiles every CEL expression of every ValidatingAdmissionPolicy in rendered
// manifests, with the CEL environment of the Kubernetes API server (issue 1313). It needs no
// cluster. `helm template`, `helm lint` and kubeconform see an expression as a plain string, so a
// bad literal (a YAML string where a CEL string literal belongs) passes them and fails only at
// `helm install`, when the API server compiles the policy.
//
// The check follows what the API server does for a policy (plugin/policy/validating): the
// variables first, in order, then the matchConditions, the validations, the auditAnnotations and
// the messageExpressions, all in the stored-expression environment of the pinned k8s.io/apiserver
// version.
//
// Usage: go run ./hack/cmd/vapcheck FILE...
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions"
	"k8s.io/apiserver/pkg/cel/environment"
)

const policyKind = "ValidatingAdmissionPolicy"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: vapcheck FILE...")
		os.Exit(2)
	}
	problems, policies := 0, 0
	for _, path := range os.Args[1:] {
		f, err := os.Open(path) // #nosec G304 G703 -- chart-check passes its own rendered files
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(2)
		}
		n, errs, err := Check(f)
		_ = f.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(2)
		}
		policies += n
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "%s: %s\n", path, e)
		}
		problems += len(errs)
	}
	if problems > 0 {
		fmt.Fprintf(os.Stderr, "%d CEL expression(s) do not compile\n", problems)
		os.Exit(1)
	}
	fmt.Printf("vapcheck: %d ValidatingAdmissionPolicy object(s) compile\n", policies)
}

// Check reads a stream of YAML documents, compiles every ValidatingAdmissionPolicy in it and
// returns the number of policies and one message per expression that does not compile. The error
// is for input it cannot read.
func Check(r io.Reader) (policies int, problems []string, err error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(r, 4096)
	for {
		var raw map[string]any
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return policies, problems, nil
			}
			return policies, problems, err
		}
		if raw == nil {
			continue
		}
		if kind, _ := raw["kind"].(string); kind != policyKind {
			continue
		}
		// v1, v1beta1 and v1alpha1 share the fields that hold CEL, so one type reads them all.
		var p admissionregistrationv1.ValidatingAdmissionPolicy
		b, err := json.Marshal(raw)
		if err == nil {
			err = json.Unmarshal(b, &p)
		}
		if err != nil {
			return policies, problems, fmt.Errorf("%s: %w", policyKind, err)
		}
		policies++
		problems = append(problems, compilePolicy(&p)...)
	}
}

// compilePolicy mirrors compilePolicy in k8s.io/apiserver/pkg/admission/plugin/policy/validating,
// and returns one message per expression that does not compile.
func compilePolicy(p *admissionregistrationv1.ValidatingAdmissionPolicy) []string {
	var out []string
	bad := func(where, expr string, err error) {
		out = append(out, fmt.Sprintf("%s %q: %s: %v (expression: %q)", policyKind, p.Name, where, err, expr))
	}

	hasParam := p.Spec.ParamKind != nil
	withAuthorizer := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: true}
	withoutAuthorizer := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: false}
	mode := environment.StoredExpressions

	c, err := cel.NewCompositedCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()))
	if err != nil {
		return []string{fmt.Sprintf("%s %q: cannot build the CEL environment: %v", policyKind, p.Name, err)}
	}
	for i, v := range p.Spec.Variables {
		res := c.CompileAndStoreVariable(&validating.Variable{Name: v.Name, Expression: v.Expression}, withAuthorizer, mode)
		if res.Error != nil {
			bad(fmt.Sprintf("spec.variables[%d] (%s)", i, v.Name), v.Expression, res.Error)
		}
	}

	// The compiler keeps each expression's result in order; CompilationErrors drops the nil ones, so
	// compile one expression at a time to name the exact entry.
	one := func(where string, acc cel.ExpressionAccessor, opts cel.OptionalVariableDeclarations) {
		for _, e := range c.CompileCondition([]cel.ExpressionAccessor{acc}, opts, mode).CompilationErrors() {
			bad(where, acc.GetExpression(), e)
		}
	}
	for i := range p.Spec.MatchConditions {
		m := p.Spec.MatchConditions[i]
		one(fmt.Sprintf("spec.matchConditions[%d] (%s)", i, m.Name), (*matchconditions.MatchCondition)(&m), withAuthorizer)
	}
	for i, v := range p.Spec.Validations {
		one(fmt.Sprintf("spec.validations[%d].expression", i),
			&validating.ValidationCondition{Expression: v.Expression, Message: v.Message, Reason: v.Reason}, withAuthorizer)
		if v.MessageExpression != "" {
			one(fmt.Sprintf("spec.validations[%d].messageExpression", i),
				&validating.MessageExpressionCondition{MessageExpression: v.MessageExpression}, withoutAuthorizer)
		}
	}
	for i, a := range p.Spec.AuditAnnotations {
		one(fmt.Sprintf("spec.auditAnnotations[%d] (%s)", i, a.Key),
			&validating.AuditAnnotationCondition{Key: a.Key, ValueExpression: a.ValueExpression}, withAuthorizer)
	}
	return out
}
