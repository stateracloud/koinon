// SPDX-License-Identifier: Apache-2.0

package credkind_test

import (
	"encoding/base64"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway/credkind"
)

// The source of truth for the kinds is the constants of type Kind in this package. The spec reads them from the
// source, not from the list, so a new kind without a rule fails here until it gets one (#1221).
var _ = Describe("the credential kind list (#1221)", func() {
	It("gives every declared kind exactly one rule, and every rule a declared kind", func() {
		declared := declaredKinds()
		Expect(declared).NotTo(BeEmpty())
		ruled := map[credkind.Kind]int{}
		for _, r := range credkind.Rules() {
			ruled[r.Kind]++
		}
		for name, k := range declared {
			Expect(ruled).To(HaveKey(k), "kind %q has no rule in the list: give it one, or a NoValue rule "+
				"with the reason it carries no value", name)
		}
		values := map[credkind.Kind]bool{}
		for _, k := range declared {
			values[k] = true
		}
		for k, n := range ruled {
			Expect(n).To(Equal(1), "kind %q has %d rules", k, n)
			Expect(values).To(HaveKey(k), "the rule of %q names no declared kind", k)
		}
	})

	It("gives each rule a known class, and a reason to each class that does not store the value", func() {
		for _, r := range credkind.Rules() {
			Expect(r.Scrub).To(BeElementOf(credkind.ByValue, credkind.ByShape, credkind.NoValue), "kind %q", r.Kind)
			if r.Scrub != credkind.ByValue {
				Expect(strings.TrimSpace(r.Why)).NotTo(BeEmpty(), "kind %q is %s and says not why", r.Kind, r.Scrub)
			}
		}
	})

	It("scrubs a kind that is not in the list by value", func() {
		r, ok := credkind.Lookup(credkind.UnlistedForTest("koinon_test_unlisted"))
		Expect(ok).To(BeFalse())
		Expect(r.Scrub).To(Equal(credkind.ByValue))
		r, ok = credkind.Lookup(credkind.Kind{})
		Expect(ok).To(BeFalse())
		Expect(r.Scrub).To(Equal(credkind.ByValue))
	})

	It("lets no other package make a kind: Kind has only unexported fields", func() {
		// A string type would let any package write credkind.Kind("x") or assign an untyped constant, and the spec
		// above would not see that kind. The compiler refuses both while every field is unexported.
		t := reflect.TypeFor[credkind.Kind]()
		Expect(t.Kind()).To(Equal(reflect.Struct))
		for f := range t.Fields() {
			Expect(f.IsExported()).To(BeFalse(), "field %s of credkind.Kind is exported", f.Name)
		}
	})
})

var _ = Describe("the passport shape (ADR 0073)", func() {
	It("matches a JWT of the passport typ, alone or after Bearer", func() {
		p := fakeJWT(`{"alg":"EdDSA","typ":"koinon-passport+jwt"}`)
		Expect(credkind.IsPassport(p)).To(BeTrue())
		Expect(credkind.IsPassport("Bearer " + p)).To(BeTrue())
		Expect(credkind.IsPassport(fakeJWT(`{"typ":"application/KOINON-PASSPORT+JWT","alg":"EdDSA"}`))).To(BeTrue())
		Expect(credkind.IsPassport("Passport "+p)).To(BeTrue(), "any one scheme word")
		Expect(credkind.IsPassport(p + " and more")).To(BeFalse())
		Expect(credkind.IsPassport("two words " + p)).To(BeFalse())
	})

	It("finds a passport that directly follows other base64url text", func() {
		p := fakeJWT(`{"alg":"EdDSA","typ":"koinon-passport+jwt"}`)
		for _, before := range []string{"honey", "key_", "eyeyey", "x.eyJ"} {
			text := before + p
			found := credkind.FindPassports(text)
			Expect(found).To(HaveLen(1), "after %q", before)
			Expect(text[found[0][0]:found[0][1]]).To(Equal(p), "after %q", before)
		}
	})

	It("finds a passport whose header starts with a line break (base64 \"ew\")", func() {
		p := fakeJWT("{\n\"alg\":\"EdDSA\",\"typ\":\"koinon-passport+jwt\"}")
		Expect(p).To(HavePrefix("ew"))
		Expect(credkind.FindPassports("echo " + p)).To(HaveLen(1))
	})

	It("does not match a JWT of another typ, or a text that only looks like one", func() {
		Expect(credkind.IsPassport(fakeJWT(`{"alg":"RS256","typ":"JWT"}`))).To(BeFalse())
		Expect(credkind.IsPassport(fakeJWT(`{"alg":"RS256"}`))).To(BeFalse())
		Expect(credkind.IsPassport("eyJub3Rqc29u.eyJ4IjoxfQ.sig")).To(BeFalse())
		Expect(credkind.FindPassports("the key and they and eyes")).To(BeEmpty())
	})

	It("finds each passport in a text and leaves other JWTs", func() {
		p := fakeJWT(`{"alg":"EdDSA","typ":"koinon-passport+jwt"}`)
		other := fakeJWT(`{"alg":"RS256","typ":"JWT"}`)
		text := `{"echo":"Bearer ` + p + `","id_token":"` + other + `"}` + p
		found := credkind.FindPassports(text)
		Expect(found).To(HaveLen(2))
		for _, f := range found {
			Expect(text[f[0]:f[1]]).To(Equal(p))
		}
	})
})

// fakeJWT returns a compact JWS with the given header, a fixed fake payload and a fake signature.
func fakeJWT(header string) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(header) + "." + enc(`{"sub":"koinon_test_not-a-real-agent"}`) + ".koinon_test_not-a-real-signature"
}

// declaredKinds parses this package and returns each kind it makes, by its name. A kind is made only with a
// composite literal Kind{"name"}; a literal of another form, or a conversion to Kind, fails the spec.
func declaredKinds() map[string]credkind.Kind {
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	Expect(err).NotTo(HaveOccurred())
	out := map[string]credkind.Kind{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				if id, ok := n.Type.(*ast.Ident); !ok || id.Name != "Kind" {
					return true
				}
				at := fset.Position(n.Pos()).String()
				Expect(n.Elts).To(HaveLen(1), "%s: make a kind as Kind{\"name\"}", at)
				lit, ok := n.Elts[0].(*ast.BasicLit)
				Expect(ok && lit.Kind == token.STRING).To(BeTrue(), "%s: the name of a kind is a string literal", at)
				v, err := strconv.Unquote(lit.Value)
				Expect(err).NotTo(HaveOccurred())
				Expect(out).NotTo(HaveKey(v), "%s: kind %q is made twice", at, v)
				out[v] = credkind.UnlistedForTest(v)
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "Kind" {
					Fail(fmt.Sprintf("%s: a conversion to Kind; make a kind as Kind{\"name\"}", fset.Position(n.Pos())))
				}
			}
			return true
		})
	}
	return out
}
