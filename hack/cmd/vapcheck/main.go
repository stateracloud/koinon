// SPDX-License-Identifier: Apache-2.0

// Command vapcheck compiles every CEL expression of every ValidatingAdmissionPolicy in rendered
// manifests, with the CEL environment of the Kubernetes API server (issue 1313). It needs no
// cluster. `helm template`, `helm lint` and kubeconform see an expression as a plain string, so a
// bad literal (a YAML string where a CEL string literal belongs) passes them and fails only at
// `helm install`, when the API server compiles the policy.
//
// The check follows what the API server does for a policy (plugin/policy/validating): the
// variables first, in order, then the matchConditions, the validations, the auditAnnotations and
// the messageExpressions. It compiles them as the API server does when it creates a policy
// (new-expression mode) for the oldest Kubernetes version the chart supports (minKubeVersion,
// the kubeVersion of charts/koinon/Chart.yaml), so a CEL library that a 1.30 server lacks fails
// here, not at helm install.
//
// With -min-distinct N the check also fails when the files hold fewer than N distinct policy
// names: a policy that no values file renders is not checked, and that must not pass in silence.
//
// Usage: go run ./hack/cmd/vapcheck [-min-distinct N] FILE...
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/util/version"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions"
	"k8s.io/apiserver/pkg/cel/environment"
)

// Keep in step with kubeVersion in charts/koinon/Chart.yaml.
var minKubeVersion = version.MajorMinor(1, 30)

const policyKind = "ValidatingAdmissionPolicy"

func main() {
	minDistinct := flag.Int("min-distinct", 0, "fail when the files hold fewer distinct policy names")
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: vapcheck [-min-distinct N] FILE...")
		os.Exit(2)
	}
	problems, policies := 0, 0
	names := map[string]bool{}
	for _, path := range flag.Args() {
		f, err := os.Open(path) // #nosec G304 G703 -- chart-check passes its own rendered files
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(2)
		}
		n, errs, found, err := scan(f)
		_ = f.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(2)
		}
		policies += n
		for _, name := range found {
			names[name] = true
		}
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "%s: %s\n", path, e)
		}
		problems += len(errs)
	}
	if problems > 0 {
		fmt.Fprintf(os.Stderr, "%d CEL expression(s) do not compile\n", problems)
		os.Exit(1)
	}
	if len(names) < *minDistinct {
		fmt.Fprintf(os.Stderr, "vapcheck: %d distinct policy name(s) rendered, expected at least %d: a policy that no values file renders is not checked\n", len(names), *minDistinct)
		os.Exit(1)
	}
	fmt.Printf("vapcheck: %d ValidatingAdmissionPolicy object(s), %d distinct, compile\n", policies, len(names))
}

// Check reads a stream of YAML documents, compiles every ValidatingAdmissionPolicy in it and
// returns the number of policies and one message per expression that does not compile. The error
// is for input it cannot read.
func Check(r io.Reader) (policies int, problems []string, err error) {
	policies, problems, _, err = scan(r)
	return policies, problems, err
}

func scan(r io.Reader) (policies int, problems []string, names []string, err error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(r, 4096)
	for {
		var raw map[string]any
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return policies, problems, names, nil
			}
			return policies, problems, names, err
		}
		if raw == nil {
			continue
		}
		apiVersion, _ := raw["apiVersion"].(string)
		if kind, _ := raw["kind"].(string); kind != policyKind || !strings.HasPrefix(apiVersion, "admissionregistration.k8s.io/") {
			continue
		}
		// v1, v1beta1 and v1alpha1 share the fields that hold CEL, so one type reads them all.
		var p admissionregistrationv1.ValidatingAdmissionPolicy
		b, err := json.Marshal(raw)
		if err == nil {
			err = json.Unmarshal(b, &p)
		}
		if err != nil {
			return policies, problems, names, fmt.Errorf("%s: %w", policyKind, err)
		}
		policies++
		names = append(names, p.Name)
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
	mode := environment.NewExpressions

	c, err := cel.NewCompositedCompiler(environment.MustBaseEnvSet(minKubeVersion))
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
