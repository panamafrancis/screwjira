package index

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// goPackage holds extracted info about a Go package.
type goPackage struct {
	Name       string
	Dir        string // relative to repo root
	Types      []string
	Interfaces []string
	Functions  []string
	Methods    []string // "TypeName.MethodName(args) returns"
}

// indexGo walks a Go repo and extracts exported symbols using go/ast.
func indexGo(repoRoot string) ([]goPackage, error) {
	pkgMap := map[string]*goPackage{}

	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}

		return indexGoFile(repoRoot, path, pkgMap)
	})
	if err != nil {
		return nil, err
	}

	var pkgs []goPackage
	for _, p := range pkgMap {
		pkgs = append(pkgs, *p)
	}
	return pkgs, nil
}

func indexGoFile(repoRoot, path string, pkgMap map[string]*goPackage) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	// Skip generated files
	if isGenerated(src) {
		return nil
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil // skip unparseable files
	}

	dir := filepath.Dir(path)
	relDir, _ := filepath.Rel(repoRoot, dir)
	pkgName := f.Name.Name

	key := relDir
	pkg, ok := pkgMap[key]
	if !ok {
		pkg = &goPackage{
			Name: pkgName,
			Dir:  relDir,
		}
		pkgMap[key] = pkg
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if !s.Name.IsExported() {
						continue
					}
					switch st := s.Type.(type) {
					case *ast.InterfaceType:
						sig := formatInterface(s.Name.Name, st)
						pkg.Interfaces = append(pkg.Interfaces, sig)
					case *ast.StructType:
						pkg.Types = append(pkg.Types, s.Name.Name)
					default:
						pkg.Types = append(pkg.Types, s.Name.Name)
					}
				}
			}
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			if d.Recv != nil {
				// Method
				recv := recvTypeName(d.Recv)
				sig := fmt.Sprintf("%s.%s%s", recv, d.Name.Name, formatFuncSignature(d.Type))
				pkg.Methods = append(pkg.Methods, sig)
			} else {
				sig := fmt.Sprintf("%s%s", d.Name.Name, formatFuncSignature(d.Type))
				pkg.Functions = append(pkg.Functions, sig)
			}
		}
	}

	return nil
}

func isGenerated(src []byte) bool {
	// Check first 1024 bytes for generation markers
	check := string(src)
	if len(check) > 1024 {
		check = check[:1024]
	}
	return strings.Contains(check, "// Code generated") ||
		strings.Contains(check, "// DO NOT EDIT")
}

func recvTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	t := recv.List[0].Type
	// Dereference pointer
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if ident, ok := t.(*ast.Ident); ok {
		return ident.Name
	}
	return "?"
}

func formatFuncSignature(ft *ast.FuncType) string {
	var b strings.Builder
	b.WriteString("(")
	if ft.Params != nil {
		b.WriteString(formatFieldList(ft.Params))
	}
	b.WriteString(")")
	if ft.Results != nil && len(ft.Results.List) > 0 {
		results := formatFieldList(ft.Results)
		if len(ft.Results.List) > 1 {
			b.WriteString(" (")
			b.WriteString(results)
			b.WriteString(")")
		} else {
			b.WriteString(" ")
			b.WriteString(results)
		}
	}
	return b.String()
}

func formatFieldList(fl *ast.FieldList) string {
	var parts []string
	for _, field := range fl.List {
		typeName := formatExpr(field.Type)
		if len(field.Names) > 0 {
			for _, name := range field.Names {
				parts = append(parts, name.Name+" "+typeName)
			}
		} else {
			parts = append(parts, typeName)
		}
	}
	return strings.Join(parts, ", ")
}

func formatExpr(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return formatExpr(e.X) + "." + e.Sel.Name
	case *ast.StarExpr:
		return "*" + formatExpr(e.X)
	case *ast.ArrayType:
		return "[]" + formatExpr(e.Elt)
	case *ast.MapType:
		return "map[" + formatExpr(e.Key) + "]" + formatExpr(e.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.Ellipsis:
		return "..." + formatExpr(e.Elt)
	case *ast.FuncType:
		return "func" + formatFuncSignature(e)
	case *ast.ChanType:
		return "chan " + formatExpr(e.Value)
	default:
		return "?"
	}
}

func formatInterface(name string, iface *ast.InterfaceType) string {
	var methods []string
	if iface.Methods != nil {
		for _, m := range iface.Methods.List {
			if len(m.Names) > 0 && m.Names[0].IsExported() {
				if ft, ok := m.Type.(*ast.FuncType); ok {
					methods = append(methods, m.Names[0].Name+formatFuncSignature(ft))
				}
			}
		}
	}
	if len(methods) == 0 {
		return name
	}
	return fmt.Sprintf("%s { %s }", name, strings.Join(methods, "; "))
}
