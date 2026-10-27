package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Every operation registered with Huma in this package calls authz.Authorize, directly or in a
// function of this package it calls, unless the comment just above huma.Register says
// "authz: public" (docs/design/08-identity-access-tenancy.md#evaluation).
func TestEveryHandlerAuthorizes(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	registered, problems := unauthorized(fset, files)
	if registered == 0 {
		t.Error("no huma.Register calls found; the check is not looking at the right code")
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// The check itself finds a handler that does not authorize.
func TestTheCheckFindsMissingAuthorize(t *testing.T) {
	src := `package api

func register(a huma.API) {
	huma.Register(a, op, func(ctx context.Context, in *struct{}) (*Out, error) {
		if err := authorize(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	})
	// authz: public — documented as open
	huma.Register(a, op, open)
	huma.Register(a, op, forgot)
	huma.Register[In, Out](a, op, func(ctx context.Context, in *In) (*Out, error) { return nil, nil })
}

func authorize(ctx context.Context) error { _, err := authz.Authorize(ctx, p, a, o); return err }
func open(ctx context.Context, in *struct{}) (*Out, error) { return nil, nil }
func forgot(ctx context.Context, in *struct{}) (*Out, error) { return helper(), nil }
func helper() *Out { return forgot2() }
func forgot2() *Out { return helper() }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	registered, problems := unauthorized(fset, []*ast.File{f})
	if registered != 4 || len(problems) != 2 || !strings.HasPrefix(problems[0], "synthetic.go:12:") || !strings.HasPrefix(problems[1], "synthetic.go:13:") {
		t.Errorf("registered %d, problems %q", registered, problems)
	}
}

// unauthorized returns how many operations the files register and where those that do not call
// authz.Authorize are.
func unauthorized(fset *token.FileSet, files []*ast.File) (registered int, problems []string) {
	funcs := map[string]*ast.FuncDecl{}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isSelector(call.Fun, "huma", "Register") || len(call.Args) == 0 {
				return true
			}
			registered++
			pos := fset.Position(call.Pos())
			if publicMarker(f, fset, pos.Line) {
				return true
			}
			if !authorizes(call.Args[len(call.Args)-1], funcs, map[string]bool{}) {
				problems = append(problems, fmt.Sprintf("%s: the handler registered here does not call authz.Authorize (mark it `// authz: public` if it must be open)", pos))
			}
			return true
		})
	}
	return registered, problems
}

func isSelector(e ast.Expr, pkg, name string) bool {
	switch idx := e.(type) {
	case *ast.IndexExpr:
		e = idx.X
	case *ast.IndexListExpr:
		e = idx.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg && sel.Sel.Name == name
}

// publicMarker reports whether a comment ending on the line above line says "authz: public".
func publicMarker(f *ast.File, fset *token.FileSet, line int) bool {
	for _, cg := range f.Comments {
		if fset.Position(cg.End()).Line == line-1 && strings.Contains(cg.Text(), "authz: public") {
			return true
		}
	}
	return false
}

// authorizes reports whether the expression is, or calls, code that calls authz.Authorize.
func authorizes(n ast.Node, funcs map[string]*ast.FuncDecl, seen map[string]bool) bool {
	if id, ok := n.(*ast.Ident); ok {
		fd := funcs[id.Name]
		if fd == nil || seen[id.Name] {
			return false
		}
		seen[id.Name] = true
		return authorizes(fd.Body, funcs, seen)
	}
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isSelector(call.Fun, "authz", "Authorize") {
			found = true
			return false
		}
		if id, ok := call.Fun.(*ast.Ident); ok && authorizes(id, funcs, seen) {
			found = true
			return false
		}
		return true
	})
	return found
}
