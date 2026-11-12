// SPDX-License-Identifier: Apache-2.0

package durable_test

import (
	"encoding/json"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/durable"
)

var update = flag.Bool("update", false, "rewrite testdata/workflow_steps.json (only with a new durable.WorkflowVersion, or when nothing changed)")

const (
	stepsFile  = "testdata/workflow_steps.json"
	dbosImport = "github.com/dbos-inc/dbos-transact-golang/dbos"
)

type stepRecord struct {
	Version   string              `json:"version"`
	Workflows map[string][]string `json:"workflows"`
}

// stepFuncs are the dbos functions that write an operation to the workflow's history, and so are a
// step of the workflow (the recorded order is what a recovered run replays).
var stepFuncs = map[string]bool{
	"RunAsStep": true, "RunAsTransaction": true, "Go": true, "Select": true, "Sleep": true,
	"Recv": true, "Send": true, "SetEvent": true, "GetEvent": true, "WriteStream": true,
	"CloseStream": true, "RunWorkflow": true, "Patch": true, "DeprecatePatch": true,
}

// pkgSource is the parsed non-test source of one package directory.
type pkgSource struct {
	funcs  map[string][]*ast.FuncDecl // by name; a method is listed under its method name
	consts map[string]string
	dbos   map[string]string // file name -> local name of the dbos import
	files  map[*ast.FuncDecl]string
	pkgs   map[string]bool // local names of the imports, so pkg.Const can be told from var.Field
}

func newPkgSource() *pkgSource {
	return &pkgSource{funcs: map[string][]*ast.FuncDecl{}, consts: map[string]string{}, dbos: map[string]string{}, files: map[*ast.FuncDecl]string{}, pkgs: map[string]bool{}}
}

// add records the declarations of f, parsed under file name name.
func (p *pkgSource) add(f *ast.File, name string) {
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if imp.Name != nil {
			p.pkgs[imp.Name.Name] = true
		} else {
			p.pkgs[path[strings.LastIndex(path, "/")+1:]] = true
		}
		if path == dbosImport {
			local := "dbos"
			if imp.Name != nil {
				local = imp.Name.Name
			}
			p.dbos[name] = local
		}
	}
	for _, decl := range f.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok {
			p.funcs[d.Name.Name] = append(p.funcs[d.Name.Name], d)
			p.files[d] = name
		}
	}
}

// resolveConsts evaluates the string constants of files (they may refer to each other, hence the passes).
func (p *pkgSource) resolveConsts(files []*ast.File) {
	for pass := 0; pass < 4; pass++ {
		for _, f := range files {
			for _, decl := range f.Decls {
				d, ok := decl.(*ast.GenDecl)
				if !ok || d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if i < len(vs.Values) {
							if v, ok := renderString(vs.Values[i], p, nil, false); ok {
								p.consts[n.Name] = v
							}
						}
					}
				}
			}
		}
	}
}

// TestWorkflowSteps fails when a registered workflow's step list changes without a bump of
// durable.WorkflowVersion (ADR 0085, decision 11). A workflow pending under the old steps could not
// continue from the middle under the new ones; the bump makes the carry-over sweep fork it from
// step 0 instead.
//
// The step lists are read from the source: every dbos.RegisterWorkflow call in the module names a
// workflow function, and the dbos calls that write history (see stepFuncs) in that function, and in
// the same-package functions it calls, are its steps in source order. dbos.WithStepName and
// dbos.WithWorkflowName arguments are resolved to their constants. So a new step, a reordered step,
// an unnamed step or a new workflow cannot pass without a decision.
//
// To record an intended change: bump durable.WorkflowVersion, check that every workflow is still
// idempotent from step 0, then run `go test ./internal/durable -run TestWorkflowSteps -update`.
func TestWorkflowSteps(t *testing.T) {
	got := stepRecord{Version: durable.WorkflowVersion, Workflows: scanWorkflows(t)}
	raw, err := os.ReadFile(stepsFile)
	if err != nil {
		t.Fatalf("read %s: %v", stepsFile, err)
	}
	var want stepRecord
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("parse %s: %v", stepsFile, err)
	}
	same := reflect.DeepEqual(got.Workflows, want.Workflows)

	if *update {
		if !same && got.Version == want.Version {
			t.Fatalf("the step lists changed but durable.WorkflowVersion is still %q: bump it first", got.Version)
		}
		out, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stepsFile, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	switch {
	case !same && got.Version == want.Version:
		t.Errorf("a registered workflow's steps changed but durable.WorkflowVersion is still %q.\n"+
			"Bump durable.WorkflowVersion (a pending workflow of the old steps is then carried over from step 0), "+
			"confirm every workflow is idempotent from step 0, and regenerate the record with\n"+
			"  go test ./internal/durable -run TestWorkflowSteps -update\nrecorded: %v\ncurrent:  %v",
			got.Version, want.Workflows, got.Workflows)
	case !same || got.Version != want.Version:
		t.Errorf("%s is stale (recorded version %q, code %q): regenerate it with\n"+
			"  go test ./internal/durable -run TestWorkflowSteps -update", stepsFile, want.Version, got.Version)
	}
}

// TestWorkflowsRegistered pins the workflows that ADR 0085 verified idempotent from step 0. A
// new workflow joins the list only after the same check, and the rule is in durable.WorkflowVersion.
func TestWorkflowsRegistered(t *testing.T) {
	found := scanWorkflows(t)
	for _, name := range []string{"koinon.notify", "koinon.trigger-fire", "koinon.trigger-run-now", "koinon.direct-message", "koinon.code-host-poll-event"} {
		if len(found[name]) == 0 {
			t.Errorf("workflow %s is not registered or has no steps", name)
		}
	}
	if len(found) != 5 {
		t.Errorf("%d workflows are registered, want the 5 verified idempotent from step 0 (ADR 0085): %v", len(found), found)
	}
}

// scanWorkflows returns the ordered step list of every workflow registered with dbos.RegisterWorkflow
// anywhere in the module, by registered name.
func scanWorkflows(t *testing.T) map[string][]string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	parsed := map[string][]*ast.File{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "web", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		parsed[filepath.Dir(path)] = append(parsed[filepath.Dir(path)], f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	pkgs := map[string]*pkgSource{}
	global := map[string]string{} // constants by bare name, for pkg.Const references
	for dir, files := range parsed {
		p := newPkgSource()
		pkgs[dir] = p
		for _, f := range files {
			p.add(f, fset.File(f.Pos()).Name())
		}
		p.resolveConsts(files)
		for k, v := range p.consts {
			global[k] = v
		}
	}

	out := map[string][]string{}
	for dir, files := range parsed {
		p := pkgs[dir]
		for _, f := range files {
			file := fset.File(f.Pos()).Name()
			local, ok := p.dbos[file]
			if !ok {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isDBOS(call.Fun, local, "RegisterWorkflow") {
					return true
				}
				pos := fset.Position(call.Pos())
				if len(call.Args) < 2 {
					t.Errorf("%s: dbos.RegisterWorkflow with too few arguments", pos)
					return true
				}
				name := ""
				for _, opt := range call.Args[2:] {
					if oc, ok := opt.(*ast.CallExpr); ok && isDBOS(oc.Fun, local, "WithWorkflowName") && len(oc.Args) == 1 {
						name, _ = renderString(oc.Args[0], p, global, true)
					}
				}
				if name == "" {
					t.Errorf("%s: dbos.RegisterWorkflow without a resolvable dbos.WithWorkflowName constant: the workflow has no stable name to record", pos)
					return true
				}
				if _, dup := out[name]; dup {
					t.Errorf("%s: workflow %q is registered twice", pos, name)
				}
				var steps []string
				if fn, ok := call.Args[1].(*ast.FuncLit); ok {
					steps = collectSteps(t, fset, fn.Body, p, file, global, map[*ast.FuncDecl]bool{})
				} else {
					id := funcName(call.Args[1])
					decls := pickMethod(p.funcs[id], call.Args[1], enclosingFunc(f, call))
					if len(decls) != 1 {
						t.Errorf("%s: cannot find the single function %q that workflow %s registers", pos, id, name)
						return true
					}
					steps = collectSteps(t, fset, decls[0].Body, p, p.files[decls[0]], global, map[*ast.FuncDecl]bool{decls[0]: true})
				}
				out[name] = steps
				return true
			})
		}
	}
	return out
}

func funcName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

// stripTypeArgs unwraps an explicit instantiation such as dbos.Recv[string].
func stripTypeArgs(fun ast.Expr) ast.Expr {
	switch x := fun.(type) {
	case *ast.IndexExpr:
		return x.X
	case *ast.IndexListExpr:
		return x.X
	}
	return fun
}

func isDBOS(fun ast.Expr, local, name string) bool {
	sel, ok := stripTypeArgs(fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == local && sel.Sel.Name == name
}

// collectSteps lists the history-writing dbos calls under body in source order, following calls to
// functions of the same package (a helper that runs a step is part of the workflow).
func collectSteps(t *testing.T, fset *token.FileSet, body ast.Node, p *pkgSource, file string, global map[string]string, seen map[*ast.FuncDecl]bool) []string {
	t.Helper()
	local := p.dbos[file]
	steps := []string{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun := stripTypeArgs(call.Fun)
		if sel, ok := fun.(*ast.SelectorExpr); ok && local != "" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
				if stepFuncs[sel.Sel.Name] {
					steps = append(steps, stepLabel(t, fset, call, sel.Sel.Name, local, p, global))
				}
				return true
			}
		}
		// A call to a function or method of this package: its steps are ours, at this point.
		if decls := p.funcs[funcName(fun)]; len(decls) == 1 && !seen[decls[0]] && decls[0].Body != nil {
			seen[decls[0]] = true
			steps = append(steps, collectSteps(t, fset, decls[0].Body, p, p.files[decls[0]], global, seen)...)
		}
		return true
	})
	return steps
}

func stepLabel(t *testing.T, fset *token.FileSet, call *ast.CallExpr, fn, local string, p *pkgSource, global map[string]string) string {
	for _, a := range call.Args {
		if oc, ok := a.(*ast.CallExpr); ok && isDBOS(oc.Fun, local, "WithStepName") && len(oc.Args) == 1 {
			if v, ok := renderString(oc.Args[0], p, global, true); ok {
				return fn + ":" + v
			}
		}
	}
	if fn == "RunAsStep" || fn == "RunAsTransaction" || fn == "Go" {
		t.Errorf("%s: dbos.%s without a resolvable dbos.WithStepName constant: give the step a stable name", fset.Position(call.Pos()), fn)
	}
	return fn
}

// renderString evaluates a string expression made of literals, constants and concatenation. A
// value that is not constant (the kind in "deliver:"+note.Kind) renders as a "<field>" placeholder
// when placeholders is true, so the record names the shape of the step without the data.
func renderString(e ast.Expr, p *pkgSource, global map[string]string, placeholders bool) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			s, err := strconv.Unquote(x.Value)
			return s, err == nil
		}
	case *ast.ParenExpr:
		return renderString(x.X, p, global, placeholders)
	case *ast.Ident:
		if v, ok := p.consts[x.Name]; ok {
			return v, true
		}
		if v, ok := global[x.Name]; ok {
			return v, true
		}
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && p.pkgs[id.Name] {
			if v, ok := global[x.Sel.Name]; ok {
				return v, true
			}
		}
		if placeholders {
			return "<" + strings.ToLower(x.Sel.Name) + ">", true
		}
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		l, ok1 := renderString(x.X, p, global, placeholders)
		r, ok2 := renderString(x.Y, p, global, placeholders)
		return l + r, ok1 && ok2
	}
	return "", false
}

// TestScanFindsSteps proves the scanner sees every kind of step, helpers included, in source order.
func TestScanFindsSteps(t *testing.T) {
	src := `package x
import "github.com/dbos-inc/dbos-transact-golang/dbos"
const (
	a = "alpha"
	b = a + "-beta"
	W = "wf"
)
func reg(c dbos.Context) { dbos.RegisterWorkflow(c, wf, dbos.WithWorkflowName(W)) }
func wf(c dbos.Context, in string) (string, error) {
	helper(c)
	dbos.Sleep(c, 0)
	return dbos.Recv[string](c, "t", 0)
}
func helper(c dbos.Context) {
	dbos.RunAsStep(c, nil, dbos.WithStepName(b))
	dbos.SetEvent(c, "k", 1)
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	p := newPkgSource()
	p.add(f, "x.go")
	p.resolveConsts([]*ast.File{f})
	wf := p.funcs["wf"][0]
	got := collectSteps(t, fset, wf.Body, p, "x.go", nil, map[*ast.FuncDecl]bool{wf: true})
	want := []string{"RunAsStep:alpha-beta", "SetEvent", "Sleep", "Recv"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

// enclosingFunc returns the top-level function of f that contains n.
func enclosingFunc(f *ast.File, n ast.Node) *ast.FuncDecl {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Pos() <= n.Pos() && n.End() <= fd.End() {
			return fd
		}
	}
	return nil
}

// pickMethod narrows several same-named methods (two types each have a "workflow") to the one
// whose receiver type x has in x.name, found without type checking from the receiver, the
// parameters or a `x := &T{}` / `x := T{}` in the enclosing function.
func pickMethod(cands []*ast.FuncDecl, ref ast.Expr, encl *ast.FuncDecl) []*ast.FuncDecl {
	if len(cands) <= 1 {
		return cands
	}
	sel, ok := ref.(*ast.SelectorExpr)
	if !ok || encl == nil {
		return cands
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return cands
	}
	typeName := ""
	baseName := func(e ast.Expr) string {
		if st, ok := e.(*ast.StarExpr); ok {
			e = st.X
		}
		if id, ok := e.(*ast.Ident); ok {
			return id.Name
		}
		return ""
	}
	fields := []*ast.FieldList{encl.Recv, encl.Type.Params}
	for _, fl := range fields {
		if fl == nil {
			continue
		}
		for _, fld := range fl.List {
			for _, n := range fld.Names {
				if n.Name == x.Name {
					typeName = baseName(fld.Type)
				}
			}
		}
	}
	ast.Inspect(encl, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok != token.DEFINE || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, l := range as.Lhs {
			if id, ok := l.(*ast.Ident); ok && id.Name == x.Name {
				rhs := as.Rhs[i]
				if u, ok := rhs.(*ast.UnaryExpr); ok {
					rhs = u.X
				}
				if cl, ok := rhs.(*ast.CompositeLit); ok {
					typeName = baseName(cl.Type)
				}
			}
		}
		return true
	})
	var out []*ast.FuncDecl
	for _, c := range cands {
		if c.Recv != nil && len(c.Recv.List) == 1 && baseName(c.Recv.List[0].Type) == typeName && typeName != "" {
			out = append(out, c)
		}
	}
	return out
}
