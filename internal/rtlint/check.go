// Package rtlint checks source rules for functions marked //tymbal:rt.
// It uses only the standard library so it can inspect all platform files on
// every CI host, including files excluded by the current build tags.
package rtlint

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Issue is a source rule violation.
type Issue struct {
	Position token.Position
	Message  string
}

func (i Issue) String() string { return fmt.Sprintf("%s: %s", i.Position, i.Message) }

type function struct {
	decl   *ast.FuncDecl
	marked bool
}

type sourceFile struct {
	file    *ast.File
	imports map[string]string
}

// Check walks every Go source file in root and reports violations in marked
// functions. Tests are excluded because they do not run on an audio thread.
func Check(root string) ([]Issue, error) {
	fset := token.NewFileSet()
	var issues []Issue
	packages := make(map[string][]sourceFile)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		imports := make(map[string]string)
		for _, spec := range file.Imports {
			name := strings.Trim(spec.Path.Value, `"`)
			alias := filepath.Base(name)
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			imports[alias] = name
		}
		packages[filepath.Dir(path)] = append(packages[filepath.Dir(path)], sourceFile{file, imports})
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, files := range packages {
		functions := make(map[string]function)
		methods := make(map[string]map[string]function)
		for _, source := range files {
			for _, decl := range source.file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				item := function{fn, marked(fn)}
				if fn.Recv == nil {
					functions[fn.Name.Name] = item
				} else if receiver := receiverType(fn.Recv.List[0].Type); receiver != "" {
					if methods[receiver] == nil {
						methods[receiver] = make(map[string]function)
					}
					methods[receiver][fn.Name.Name] = item
				}
			}
		}
		for _, source := range files {
			for _, decl := range source.file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok && marked(fn) {
					checkFunction(fset, fn, source.imports, functions, methods, &issues)
				}
			}
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		a, b := issues[i].Position, issues[j].Position
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})
	return issues, nil
}

func marked(fn *ast.FuncDecl) bool {
	if fn.Doc == nil {
		return false
	}
	for _, comment := range fn.Doc.List {
		if strings.TrimSpace(comment.Text) == "//tymbal:rt" {
			return true
		}
	}
	return false
}

func receiverType(expr ast.Expr) string {
	if ptr, ok := expr.(*ast.StarExpr); ok {
		expr = ptr.X
	}
	if name, ok := expr.(*ast.Ident); ok {
		return name.Name
	}
	return ""
}

func checkFunction(fset *token.FileSet, fn *ast.FuncDecl, imports map[string]string, functions map[string]function, methods map[string]map[string]function, issues *[]Issue) {
	report := func(node ast.Node, message string) {
		*issues = append(*issues, Issue{fset.Position(node.Pos()), message})
	}
	receiverName, receiver := "", ""
	if fn.Recv != nil {
		receiver = receiverType(fn.Recv.List[0].Type)
		if len(fn.Recv.List[0].Names) != 0 {
			receiverName = fn.Recv.List[0].Names[0].Name
		}
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.GoStmt:
			report(n, "goroutine on real-time path")
		case *ast.SendStmt:
			report(n, "channel send on real-time path")
		case *ast.SelectStmt:
			report(n, "select on real-time path")
		case *ast.DeferStmt:
			if fn.Name.Name != "callCallback" {
				report(n, "defer on real-time path")
			}
		case *ast.UnaryExpr:
			if n.Op == token.ARROW {
				report(n, "channel receive on real-time path")
			}
		case *ast.BinaryExpr:
			if n.Op == token.ADD && (stringLiteral(n.X) || stringLiteral(n.Y)) {
				report(n, "string concatenation on real-time path")
			}
		case *ast.CallExpr:
			switch call := n.Fun.(type) {
			case *ast.Ident:
				switch call.Name {
				case "make", "new", "append":
					report(n, call.Name+" on real-time path")
				case "string":
					if len(n.Args) == 1 {
						if _, ok := n.Args[0].(*ast.CallExpr); ok {
							report(n, "possible byte-to-string conversion on real-time path")
						}
					}
				default:
					if other, ok := functions[call.Name]; ok && !other.marked {
						report(n, "call to unmarked local function "+call.Name)
					}
				}
			case *ast.SelectorExpr:
				switch call.Sel.Name {
				case "Lock", "Unlock", "RLock", "RUnlock":
					report(n, "lock operation on real-time path")
				}
				if pkg, ok := call.X.(*ast.Ident); ok {
					if path, imported := imports[pkg.Name]; imported {
						if path == "fmt" || path == "log" || (path == "errors" && call.Sel.Name == "New") ||
							(path == "time" && (call.Sel.Name == "Sleep" || call.Sel.Name == "After")) {
							report(n, "forbidden call "+path+"."+call.Sel.Name)
						}
					} else if pkg.Name == receiverName {
						if other, exists := methods[receiver][call.Sel.Name]; exists && !other.marked {
							report(n, "call to unmarked local method "+call.Sel.Name)
						}
					}
				}
			}
		}
		return true
	})
}

func stringLiteral(expr ast.Expr) bool {
	literal, ok := expr.(*ast.BasicLit)
	return ok && literal.Kind == token.STRING
}
