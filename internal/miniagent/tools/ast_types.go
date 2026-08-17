package tools

import "go/ast"

func kindOfType(expr ast.Expr) string {
	switch expr.(type) {
	case *ast.InterfaceType:
		return "interface"
	case *ast.StructType:
		return "struct"
	default:
		return "type"
	}
}

// typeString renders a receiver type for display: *T and T both show their name;
// generic receivers (Box[T], Box[T, U]) strip the type arguments; anything else
// renders as "?" (rare, legal Go: unnamed receiver base types).
func typeString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return typeString(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr, *ast.IndexListExpr:
		return typeString(identOf(t))
	}
	return "?"
}

func identOf(expr ast.Expr) ast.Expr {
	switch t := expr.(type) {
	case *ast.IndexExpr:
		return t.X
	case *ast.IndexListExpr:
		return t.X
	}
	return expr
}
