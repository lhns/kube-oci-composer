package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestEveryFlagIsActuallyRead: a flag variable referenced only where it is declared and bound is
// parsed and ignored, which the compiler cannot catch (--build-poll-interval once shipped so, and
// the chart derived gcDelay from a value the binary never used).
func TestEveryFlagIsActuallyRead(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing main.go: %v", err)
	}

	// Flag variables, by the name taken in flag.XxxVar(&name, "flag-name", ...).
	bound := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "flag" {
			return true
		}
		unary, ok := call.Args[0].(*ast.UnaryExpr)
		if !ok || unary.Op != token.AND {
			return true
		}
		target, ok := unary.X.(*ast.Ident)
		if !ok {
			return true
		}
		name, ok := call.Args[1].(*ast.BasicLit)
		if !ok {
			return true
		}
		bound[target.Name] = name.Value
		return true
	})

	if len(bound) == 0 {
		t.Fatal("no flag bindings found; this test would pass vacuously")
	}

	uses := map[string]int{}
	ast.Inspect(file, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			uses[id.Name]++
		}
		return true
	})

	for varName, flagName := range bound {
		// The declaration and the flag.XxxVar argument; anything more is a real use.
		if uses[varName] <= 2 {
			t.Errorf("%s is bound to %s and then never read: it is parsed, documented and "+
				"discarded, so setting it changes nothing. Pass it to whatever should honour it, "+
				"or delete the flag.", varName, flagName)
		}
	}
}
