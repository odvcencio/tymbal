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

type packageRules struct {
	functions      map[string][]function
	methods        map[string]map[string][]function
	mapFields      map[string]map[string]bool
	mapTypes       map[string]bool
	stringTypes    map[string]bool
	byteSliceTypes map[string]bool
	channelTypes   map[string]bool
	arenaTypes     map[string]map[string]bool
	globals        map[string]bool
	stringGlobals  map[string]bool
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
	markedPackages := packageMarkers(root, packages)
	for _, files := range packages {
		rules := packageRules{
			functions:      make(map[string][]function),
			methods:        make(map[string]map[string][]function),
			mapFields:      make(map[string]map[string]bool),
			mapTypes:       make(map[string]bool),
			stringTypes:    make(map[string]bool),
			byteSliceTypes: make(map[string]bool),
			channelTypes:   make(map[string]bool),
			arenaTypes:     make(map[string]map[string]bool),
			globals:        make(map[string]bool),
			stringGlobals:  make(map[string]bool),
		}
		for changed := true; changed; {
			changed = false
			for _, source := range files {
				for _, decl := range source.file.Decls {
					if gen, ok := decl.(*ast.GenDecl); ok {
						for _, spec := range gen.Specs {
							if typ, ok := spec.(*ast.TypeSpec); ok {
								if isMapType(typ.Type) || isIdentIn(typ.Type, rules.mapTypes) {
									changed = addNamedType(rules.mapTypes, typ.Name.Name) || changed
								}
								if isStringType(typ.Type) || isIdentIn(typ.Type, rules.stringTypes) {
									changed = addNamedType(rules.stringTypes, typ.Name.Name) || changed
								}
								if isByteSliceType(typ.Type) || isIdentIn(typ.Type, rules.byteSliceTypes) {
									changed = addNamedType(rules.byteSliceTypes, typ.Name.Name) || changed
								}
								if isChanType(typ.Type) || isIdentIn(typ.Type, rules.channelTypes) {
									changed = addNamedType(rules.channelTypes, typ.Name.Name) || changed
								}
							}
						}
					}
				}
			}
		}
		for _, source := range files {
			for _, decl := range source.file.Decls {
				switch n := decl.(type) {
				case *ast.FuncDecl:
					item := function{n, marked(n)}
					if n.Recv == nil {
						rules.functions[n.Name.Name] = append(rules.functions[n.Name.Name], item)
					} else if receiver := receiverType(n.Recv.List[0].Type); receiver != "" {
						if rules.methods[receiver] == nil {
							rules.methods[receiver] = make(map[string][]function)
						}
						rules.methods[receiver][n.Name.Name] = append(rules.methods[receiver][n.Name.Name], item)
					}
				case *ast.GenDecl:
					for _, spec := range n.Specs {
						switch value := spec.(type) {
						case *ast.TypeSpec:
							if isMapType(value.Type) || isIdentIn(value.Type, rules.mapTypes) {
								rules.mapTypes[value.Name.Name] = true
							}
							if fields := structMapFields(value.Type, rules.mapTypes); len(fields) != 0 {
								rules.mapFields[value.Name.Name] = fields
							}
							if fields := structArenaDataFields(value.Type); len(fields) != 0 {
								rules.arenaTypes[value.Name.Name] = fields
							}
						case *ast.ValueSpec:
							for i, name := range value.Names {
								rules.globals[name.Name] = true
								if isMapType(value.Type) || isIdentIn(value.Type, rules.mapTypes) || i < len(value.Values) && isMapExpr(value.Values[i], nil, rules.mapTypes, rules.mapFields) {
									rules.globals["map:"+name.Name] = true
								}
								if isStringType(value.Type) || isIdentIn(value.Type, rules.stringTypes) || i < len(value.Values) && stringLiteral(value.Values[i]) {
									rules.stringGlobals[name.Name] = true
								}
							}
						}
					}
				}
			}
		}
		for _, source := range files {
			for _, decl := range source.file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok {
					auditArenaPointers(fset, fn, source.imports, rules, &issues)
					if marked(fn) {
						checkFunction(fset, fn, source.imports, rules, markedPackages, &issues)
					}
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

func packageMarkers(root string, packages map[string][]sourceFile) map[string]map[string]bool {
	modulePath := ""
	if data, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "module" {
				modulePath = strings.Trim(fields[1], `"`)
				break
			}
		}
	}
	if modulePath == "" {
		return nil
	}
	markers := make(map[string]map[string]bool, len(packages))
	for dir, files := range packages {
		relative, err := filepath.Rel(root, dir)
		if err != nil {
			continue
		}
		importPath := modulePath
		if relative != "." {
			importPath += "/" + filepath.ToSlash(relative)
		}
		functions := make(map[string]bool)
		for _, source := range files {
			for _, decl := range source.file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
					functions[fn.Name.Name] = marked(fn)
				}
			}
		}
		markers[importPath] = functions
	}
	return markers
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

func checkFunction(fset *token.FileSet, fn *ast.FuncDecl, imports map[string]string, rules packageRules, markedPackages map[string]map[string]bool, issues *[]Issue) {
	report := func(node ast.Node, message string) {
		*issues = append(*issues, Issue{fset.Position(node.Pos()), message})
	}
	receiverName, receiver := "", ""
	localTypes := realtimeTypeNames(fn)
	if fn.Recv != nil {
		receiver = receiverType(fn.Recv.List[0].Type)
		if len(fn.Recv.List[0].Names) != 0 {
			receiverName = fn.Recv.List[0].Names[0].Name
		}
	}
	parents := parentNodes(fn.Body)
	mapNames := realtimeMapNames(fn, rules)
	stringNames, byteNames := realtimeStringNames(fn, rules)
	channelNames := realtimeChannelNames(fn, rules)
	localNames := realtimeLocalNames(fn)
	escapedComposites := escapingComposites(fn, parents, localNames)
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CompositeLit:
			if isMapType(n.Type) || isIdentIn(n.Type, rules.mapTypes) {
				report(n, "map literal allocation on real-time path")
			}
			if escapedComposites[n] {
				report(n, "escaping composite literal on real-time path")
			}
		case *ast.GoStmt:
			report(n, "goroutine on real-time path")
		case *ast.SendStmt:
			report(n, "channel send on real-time path")
		case *ast.SelectStmt:
			report(n, "select on real-time path")
		case *ast.DeferStmt:
			if fn.Name.Name != "callCallback" || !isRecoverWrapper(n) {
				report(n, "defer on real-time path")
			}
		case *ast.UnaryExpr:
			if n.Op == token.ARROW {
				report(n, "channel receive on real-time path")
			}
		case *ast.BinaryExpr:
			if n.Op == token.ADD && (isStringExpr(n.X, stringNames) || isStringExpr(n.Y, stringNames)) {
				report(n, "string concatenation on real-time path")
			}
		case *ast.IndexExpr:
			if isMapExpr(n.X, mapNames, rules.mapTypes, rules.mapFields) {
				report(n, "map access on real-time path")
			}
		case *ast.RangeStmt:
			if isMapExpr(n.X, mapNames, rules.mapTypes, rules.mapFields) {
				report(n, "map access on real-time path")
			} else if isChannelExpr(n.X, channelNames, rules.channelTypes) {
				report(n, "channel receive on real-time path")
			}
		case *ast.CallExpr:
			switch call := n.Fun.(type) {
			case *ast.Ident:
				switch call.Name {
				case "make", "new", "append":
					report(n, call.Name+" on real-time path")
				case "delete", "clear":
					if len(n.Args) != 0 && isMapExpr(n.Args[0], mapNames, rules.mapTypes, rules.mapFields) {
						report(n, "map access on real-time path")
					}
				case "close":
					if len(n.Args) != 0 && isChannelExpr(n.Args[0], channelNames, rules.channelTypes) {
						report(n, "channel operation on real-time path")
					}
				default:
					if others := rules.functions[call.Name]; len(others) != 0 && hasUnmarked(others) {
						report(n, "call to unmarked local function "+call.Name)
					} else if path, imported := imports["."]; imported && !allowedPackageCall(path, call.Name) {
						report(n, "call outside real-time allowlist "+path+"."+call.Name)
					}
				}
				if call.Name == "string" && len(n.Args) == 1 && isByteSliceExpr(n.Args[0], byteNames) {
					report(n, "byte-to-string conversion on real-time path")
				}
				if call.Name == "[]byte" && len(n.Args) == 1 && isStringExpr(n.Args[0], stringNames) {
					report(n, "string-to-byte conversion on real-time path")
				}
			case *ast.ArrayType:
				if isByteSliceType(call) && len(n.Args) == 1 && isStringExpr(n.Args[0], stringNames) {
					report(n, "string-to-byte conversion on real-time path")
				}
			case *ast.SelectorExpr:
				switch call.Sel.Name {
				case "Lock", "Unlock", "RLock", "RUnlock", "TryLock", "TryRLock":
					report(n, "lock operation on real-time path")
				}
				if pkg, ok := call.X.(*ast.Ident); ok {
					if path, imported := imports[pkg.Name]; imported {
						if path == "fmt" || path == "log" || (path == "errors" && call.Sel.Name == "New") ||
							(path == "time" && (call.Sel.Name == "Sleep" || call.Sel.Name == "After")) {
							report(n, "forbidden call "+path+"."+call.Sel.Name)
						} else if _, conversion := importedTypeConversion(n.Fun, imports); conversion {
							// Imported type conversions are not calls into package code.
						} else if functions, internal := markedPackages[path]; internal {
							if !functions[call.Sel.Name] {
								report(n, "call to unmarked imported function "+path+"."+call.Sel.Name)
							}
						} else if !allowedPackageCall(path, call.Sel.Name) {
							report(n, "call outside real-time allowlist "+path+"."+call.Sel.Name)
						}
					} else if pkg.Name == receiverName {
						if others, exists := rules.methods[receiver][call.Sel.Name]; exists && hasUnmarked(others) {
							report(n, "call to unmarked local method "+call.Sel.Name)
						}
					} else if localType := localTypes[pkg.Name]; localType != "" {
						if others, exists := rules.methods[localType][call.Sel.Name]; exists && hasUnmarked(others) {
							report(n, "call to unmarked local method "+call.Sel.Name)
						}
					}
				}
			}
		case *ast.SelectorExpr:
			if pkg, ok := n.X.(*ast.Ident); ok && imports[pkg.Name] == "sync" && (n.Sel.Name == "Mutex" || n.Sel.Name == "RWMutex") {
				report(n, "sync mutex type on real-time path")
			}
		}
		return true
	})
}

func hasUnmarked(functions []function) bool {
	for _, fn := range functions {
		if !fn.marked {
			return true
		}
	}
	return false
}

func allowedPackageCall(path, name string) bool {
	switch path {
	case "sync/atomic", "math", "math/bits", "unsafe":
		return true
	case "errors":
		return name == "Is"
	case "runtime":
		return name == "KeepAlive"
	case "time":
		return name == "Since"
	case "syscall":
		return name == "Syscall" || name == "Syscall6" || name == "SyscallN" || name == "RawSyscall" || name == "RawSyscall6"
	default:
		return false
	}
}

func importedTypeConversion(expr ast.Expr, imports map[string]string) (string, bool) {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	base, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	path, ok := imports[base.Name]
	if !ok {
		return "", false
	}
	// These imported types are used as conversions on the real-time path.
	switch path + "." + sel.Sel.Name {
	case "time.Duration", "syscall.Errno", "m31labs.dev/tymbal/internal/format.Format":
		return path + "." + sel.Sel.Name, true
	}
	return "", false
}

func parentNodes(root ast.Node) map[ast.Node]ast.Node {
	parents := make(map[ast.Node]ast.Node)
	visitor := &parentVisitor{parents: parents}
	ast.Walk(visitor, root)
	return parents
}

type parentVisitor struct {
	parents map[ast.Node]ast.Node
	stack   []ast.Node
}

func (v *parentVisitor) Visit(node ast.Node) ast.Visitor {
	if node == nil {
		v.stack = v.stack[:len(v.stack)-1]
		return v
	}
	if len(v.stack) != 0 {
		v.parents[node] = v.stack[len(v.stack)-1]
	}
	v.stack = append(v.stack, node)
	return v
}

func realtimeLocalNames(fn *ast.FuncDecl) map[string]bool {
	names := make(map[string]bool)
	if fn.Recv != nil {
		for _, field := range fn.Recv.List {
			for _, name := range field.Names {
				names[name.Name] = true
			}
		}
	}
	if fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			for _, name := range field.Names {
				names[name.Name] = true
			}
		}
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				for _, lhs := range n.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						names[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for _, name := range n.Names {
				names[name.Name] = true
			}
		case *ast.RangeStmt:
			for _, expr := range []ast.Expr{n.Key, n.Value} {
				if id, ok := expr.(*ast.Ident); ok {
					names[id.Name] = true
				}
			}
		}
		return true
	})
	return names
}

func realtimeTypeNames(fn *ast.FuncDecl) map[string]string {
	names := make(map[string]string)
	add := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, field := range fields.List {
			name := typeName(field.Type)
			for _, ident := range field.Names {
				names[ident.Name] = name
			}
		}
	}
	add(fn.Recv)
	add(fn.Type.Params)
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.ValueSpec:
			if name := typeName(n.Type); name != "" {
				for _, ident := range n.Names {
					names[ident.Name] = name
				}
			}
		case *ast.AssignStmt:
			for i, rhs := range n.Rhs {
				if i >= len(n.Lhs) {
					break
				}
				lhs, ok := n.Lhs[i].(*ast.Ident)
				if !ok {
					continue
				}
				var expr ast.Expr = rhs
				if unary, ok := expr.(*ast.UnaryExpr); ok && unary.Op == token.AND {
					expr = unary.X
				}
				if lit, ok := expr.(*ast.CompositeLit); ok {
					names[lhs.Name] = typeName(lit.Type)
				} else if id, ok := expr.(*ast.Ident); ok && names[id.Name] != "" {
					names[lhs.Name] = names[id.Name]
				}
			}
		}
		return true
	})
	return names
}

func escapingComposites(fn *ast.FuncDecl, parents map[ast.Node]ast.Node, locals map[string]bool) map[*ast.CompositeLit]bool {
	escaped := make(map[*ast.CompositeLit]bool)
	origins := make(map[string][]*ast.CompositeLit)
	addressableOrigins := make(map[string][]*ast.CompositeLit)
	allCompositeLiterals := func(expr ast.Expr) []*ast.CompositeLit {
		var found []*ast.CompositeLit
		ast.Inspect(expr, func(node ast.Node) bool {
			if lit, ok := node.(*ast.CompositeLit); ok {
				found = append(found, lit)
			}
			return true
		})
		return found
	}
	collect := func(expr ast.Expr) []*ast.CompositeLit {
		var found []*ast.CompositeLit
		ast.Inspect(expr, func(node ast.Node) bool {
			lit, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if compositeOwnsStorage(lit, parents) {
				found = append(found, lit)
			}
			return true
		})
		if id, ok := expr.(*ast.Ident); ok {
			found = append(found, origins[id.Name]...)
		}
		if unary, ok := expr.(*ast.UnaryExpr); ok && unary.Op == token.AND {
			if id, ok := unary.X.(*ast.Ident); ok {
				found = append(found, addressableOrigins[id.Name]...)
			}
		}
		return found
	}
	mark := func(lits []*ast.CompositeLit) {
		for _, lit := range lits {
			escaped[lit] = true
		}
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			for i, rhs := range n.Rhs {
				lits := collect(rhs)
				lhsIndex := i
				if lhsIndex >= len(n.Lhs) {
					lhsIndex = len(n.Lhs) - 1
				}
				if lhsIndex < 0 {
					continue
				}
				lhs := n.Lhs[lhsIndex]
				if id, ok := lhs.(*ast.Ident); ok && id.Name == "_" {
					continue
				}
				if id, ok := lhs.(*ast.Ident); ok && locals[id.Name] {
					if len(lits) != 0 {
						origins[id.Name] = append(origins[id.Name][:0], lits...)
					}
					all := append(allCompositeLiterals(rhs), lits...)
					if source, ok := rhs.(*ast.Ident); ok {
						all = append(all, addressableOrigins[source.Name]...)
					}
					addressableOrigins[id.Name] = append(addressableOrigins[id.Name][:0], all...)
				} else if len(lits) != 0 {
					mark(lits)
				}
			}
		case *ast.ValueSpec:
			for i, rhs := range n.Values {
				if i >= len(n.Names) {
					break
				}
				lits := collect(rhs)
				name := n.Names[i].Name
				if locals[name] {
					if len(lits) != 0 {
						origins[name] = append(origins[name][:0], lits...)
					}
					all := append(allCompositeLiterals(rhs), lits...)
					if source, ok := rhs.(*ast.Ident); ok {
						all = append(all, addressableOrigins[source.Name]...)
					}
					addressableOrigins[name] = append(addressableOrigins[name][:0], all...)
				} else if len(lits) != 0 {
					mark(lits)
				}
			}
		case *ast.ReturnStmt:
			for _, result := range n.Results {
				mark(collect(result))
			}
		case *ast.CallExpr:
			if _, ok := n.Fun.(*ast.Ident); ok {
				if len(n.Args) != 0 {
					for _, arg := range n.Args {
						if isNoEscapeBuiltin(n.Fun, arg) {
							continue
						}
						mark(collect(arg))
					}
				}
			} else {
				for _, arg := range n.Args {
					mark(collect(arg))
				}
			}
		case *ast.SendStmt:
			mark(collect(n.Value))
		}
		return true
	})
	return escaped
}

func compositeOwnsStorage(lit *ast.CompositeLit, parents map[ast.Node]ast.Node) bool {
	switch lit.Type.(type) {
	case *ast.MapType:
		return true
	case *ast.ArrayType:
		if array, ok := lit.Type.(*ast.ArrayType); ok && array.Len == nil {
			return true
		}
		if unary, ok := parents[lit].(*ast.UnaryExpr); ok && unary.Op == token.AND {
			return true
		}
	}
	if unary, ok := parents[lit].(*ast.UnaryExpr); ok && unary.Op == token.AND {
		return true
	}
	return false
}

func isNoEscapeBuiltin(fun ast.Expr, _ ast.Expr) bool {
	id, ok := fun.(*ast.Ident)
	if !ok {
		return false
	}
	switch id.Name {
	case "len", "cap", "clear", "copy":
		return true
	}
	return false
}

func isRecoverWrapper(stmt *ast.DeferStmt) bool {
	call, ok := stmt.Call.Fun.(*ast.FuncLit)
	if !ok || len(stmt.Call.Args) != 0 || call.Body == nil {
		return false
	}
	recoverCalls := 0
	ast.Inspect(call.Body, func(node ast.Node) bool {
		if expression, ok := node.(*ast.CallExpr); ok {
			id, ok := expression.Fun.(*ast.Ident)
			if ok && id.Name == "recover" {
				recoverCalls++
			}
		}
		return true
	})
	return recoverCalls == 1
}

func structMapFields(expr ast.Expr, mapTypes map[string]bool) map[string]bool {
	st, ok := expr.(*ast.StructType)
	if !ok || st.Fields == nil {
		return nil
	}
	fields := make(map[string]bool)
	for _, field := range st.Fields.List {
		if isMapType(field.Type) || isIdentIn(field.Type, mapTypes) {
			for _, name := range field.Names {
				fields[name.Name] = true
			}
		}
	}
	return fields
}

func structArenaDataFields(expr ast.Expr) map[string]bool {
	st, ok := expr.(*ast.StructType)
	if !ok || st.Fields == nil {
		return nil
	}
	hasArena, hasData := false, false
	fields := make(map[string]bool)
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			continue
		}
		for _, name := range field.Names {
			switch name.Name {
			case "arena":
				hasArena = true
			case "data":
				hasData = true
				fields[name.Name] = true
			}
		}
	}
	if !hasArena || !hasData {
		return nil
	}
	return fields
}

func auditArenaPointers(fset *token.FileSet, fn *ast.FuncDecl, imports map[string]string, rules packageRules, issues *[]Issue) {
	if fn.Body == nil {
		return
	}
	localTypes := make(map[string]string)
	if fn.Recv != nil {
		for _, field := range fn.Recv.List {
			for _, name := range field.Names {
				localTypes[name.Name] = typeName(field.Type)
			}
		}
	}
	if fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			for _, name := range field.Names {
				localTypes[name.Name] = typeName(field.Type)
			}
		}
	}
	arenaVars, dataVars := make(map[string]bool), make(map[string]bool)
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if assign, ok := node.(*ast.AssignStmt); ok {
			for i, rhs := range assign.Rhs {
				if i >= len(assign.Lhs) {
					break
				}
				lhs, ok := assign.Lhs[i].(*ast.Ident)
				if !ok {
					continue
				}
				if callsArenaAllocator(rhs, imports) {
					arenaVars[lhs.Name] = true
				}
				if isArenaSlice(rhs, arenaVars) {
					dataVars[lhs.Name] = true
				}
				if call, ok := rhs.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Take" {
						dataVars[lhs.Name] = true
					}
				}
				if typeName, ok := assignedType(rhs, localTypes); ok {
					localTypes[lhs.Name] = typeName
				}
			}
		}
		if spec, ok := node.(*ast.ValueSpec); ok {
			for i, rhs := range spec.Values {
				if i >= len(spec.Names) {
					break
				}
				name := spec.Names[i].Name
				if callsArenaAllocator(rhs, imports) {
					arenaVars[name] = true
				}
				if isArenaSlice(rhs, arenaVars) {
					dataVars[name] = true
				}
				if typeName, ok := assignedType(rhs, localTypes); ok {
					localTypes[name] = typeName
				}
			}
		}
		return true
	})
	unsafeVars, arenaUnsafeVars := make(map[string]bool), make(map[string]bool)
	trackUnsafe := func(name string, expr ast.Expr) {
		if containsUnsafePointer(expr, imports) {
			unsafeVars[name] = true
			arenaUnsafeVars[name] = isArenaPointer(expr, arenaVars, dataVars, localTypes, rules.arenaTypes, rules.globals, imports)
			return
		}
		if id, ok := expr.(*ast.Ident); ok && unsafeVars[id.Name] {
			unsafeVars[name] = true
			arenaUnsafeVars[name] = arenaUnsafeVars[id.Name]
		}
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			for i, rhs := range n.Rhs {
				if i < len(n.Lhs) {
					if lhs, ok := n.Lhs[i].(*ast.Ident); ok {
						trackUnsafe(lhs.Name, rhs)
					}
				}
			}
		case *ast.ValueSpec:
			for i, rhs := range n.Values {
				if i < len(n.Names) {
					trackUnsafe(n.Names[i].Name, rhs)
				}
			}
		}
		return true
	})
	seen := make(map[ast.Node]bool)
	check := func(node ast.Node, value ast.Expr) {
		if seen[node] || !containsUnsafePointer(value, imports) && !containsUnsafeVariable(value, unsafeVars) || isArenaPointer(value, arenaVars, dataVars, localTypes, rules.arenaTypes, rules.globals, imports) || isArenaUnsafeVariable(value, arenaUnsafeVars) {
			return
		}
		seen[node] = true
		*issues = append(*issues, Issue{fset.Position(node.Pos()), "OS-visible pointer storage must reference arena memory"})
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				if i < len(n.Rhs) {
					if _, ok := lhs.(*ast.SelectorExpr); ok {
						check(lhs, n.Rhs[i])
					}
				}
			}
		case *ast.KeyValueExpr:
			if key, ok := n.Key.(*ast.Ident); ok && (key.Name == "buf" || key.Name == "ptr") {
				check(n, n.Value)
			}
		}
		return true
	})
}

func typeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	id, _ := expr.(*ast.Ident)
	if id == nil {
		return ""
	}
	return id.Name
}

func assignedType(expr ast.Expr, names map[string]string) (string, bool) {
	switch n := expr.(type) {
	case *ast.Ident:
		name, ok := names[n.Name]
		return name, ok
	case *ast.UnaryExpr:
		if n.Op == token.AND {
			return assignedType(n.X, names)
		}
	}
	return "", false
}

func callsArenaAllocator(expr ast.Expr, imports map[string]string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := fun.X.(*ast.Ident); ok {
			path := imports[pkg.Name]
			return path == "syscall" && fun.Sel.Name == "Mmap" || strings.HasSuffix(path, "/ring") && fun.Sel.Name == "NewArena"
		}
		return fun.Sel.Name == "NewArena"
	case *ast.Ident:
		return fun.Name == "NewArena"
	}
	return false
}

func isArenaSlice(expr ast.Expr, arenaVars map[string]bool) bool {
	switch n := expr.(type) {
	case *ast.SliceExpr:
		id, ok := n.X.(*ast.Ident)
		return ok && arenaVars[id.Name]
	case *ast.CallExpr:
		if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Take" {
			return true
		}
	}
	return false
}

func containsUnsafePointer(expr ast.Expr, imports map[string]string) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Pointer" {
			if pkg, ok := sel.X.(*ast.Ident); ok && imports[pkg.Name] == "unsafe" {
				found = true
			}
		} else if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "Pointer" && imports["."] == "unsafe" {
			found = true
		}
		return true
	})
	return found
}

func isArenaPointer(expr ast.Expr, arenaVars, dataVars map[string]bool, localTypes map[string]string, arenaTypes map[string]map[string]bool, globals map[string]bool, imports map[string]string) bool {
	var pointerSource ast.Expr
	ast.Inspect(expr, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Pointer" {
			if pkg, ok := sel.X.(*ast.Ident); ok && imports[pkg.Name] == "unsafe" {
				pointerSource = call.Args[0]
				return false
			}
		} else if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "Pointer" && imports["."] == "unsafe" {
			pointerSource = call.Args[0]
			return false
		}
		return true
	})
	if pointerSource == nil {
		return false
	}
	if address, ok := pointerSource.(*ast.UnaryExpr); ok && address.Op == token.AND {
		switch source := address.X.(type) {
		case *ast.IndexExpr:
			if id, ok := source.X.(*ast.Ident); ok && (arenaVars[id.Name] || dataVars[id.Name] || globals[id.Name]) {
				return true
			}
			if field, ok := source.X.(*ast.SelectorExpr); ok && arenaDataSelector(field, localTypes, arenaTypes) {
				return true
			}
		case *ast.SelectorExpr:
			return arenaDataSelector(source, localTypes, arenaTypes)
		}
	}
	return false
}

func containsUnsafeVariable(expr ast.Expr, variables map[string]bool) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		if id, ok := node.(*ast.Ident); ok && variables[id.Name] {
			found = true
			return false
		}
		return !found
	})
	return found
}

func isArenaUnsafeVariable(expr ast.Expr, variables map[string]bool) bool {
	id, ok := expr.(*ast.Ident)
	return ok && variables[id.Name]
}

func arenaDataSelector(expr *ast.SelectorExpr, localTypes map[string]string, arenaTypes map[string]map[string]bool) bool {
	if base, ok := expr.X.(*ast.Ident); ok {
		typeName := localTypes[base.Name]
		return arenaTypes[typeName][expr.Sel.Name]
	}
	return false
}

func isMapType(expr ast.Expr) bool {
	_, ok := expr.(*ast.MapType)
	return ok
}

func isChanType(expr ast.Expr) bool {
	_, ok := expr.(*ast.ChanType)
	return ok
}

func addNamedType(names map[string]bool, name string) bool {
	if names[name] {
		return false
	}
	names[name] = true
	return true
}

func isIdentIn(expr ast.Expr, names map[string]bool) bool {
	id, ok := expr.(*ast.Ident)
	if !ok || names == nil {
		return false
	}
	return names[id.Name]
}

func isMapExpr(expr ast.Expr, names, mapTypes map[string]bool, fields map[string]map[string]bool) bool {
	switch n := expr.(type) {
	case *ast.Ident:
		return names != nil && names[n.Name] || mapTypes != nil && mapTypes[n.Name]
	case *ast.CompositeLit:
		return isMapType(n.Type) || isIdentIn(n.Type, mapTypes)
	case *ast.CallExpr:
		id, ok := n.Fun.(*ast.Ident)
		return ok && id.Name == "make" && len(n.Args) != 0 && (isMapType(n.Args[0]) || isIdentIn(n.Args[0], mapTypes))
	case *ast.ParenExpr:
		return isMapExpr(n.X, names, mapTypes, fields)
	case *ast.SelectorExpr:
		if fields != nil {
			for _, members := range fields {
				if members[n.Sel.Name] {
					return true
				}
			}
		}
	}
	return false
}

func realtimeMapNames(fn *ast.FuncDecl, rules packageRules) map[string]bool {
	names := make(map[string]bool)
	addFields := func(list *ast.FieldList) {
		if list == nil {
			return
		}
		for _, field := range list.List {
			if isMapType(field.Type) || isIdentIn(field.Type, rules.mapTypes) {
				for _, name := range field.Names {
					names[name.Name] = true
				}
			}
		}
	}
	if fn.Recv != nil {
		addFields(fn.Recv)
	}
	addFields(fn.Type.Params)
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if spec, ok := node.(*ast.ValueSpec); ok && (isMapType(spec.Type) || isIdentIn(spec.Type, rules.mapTypes)) {
			for _, name := range spec.Names {
				names[name.Name] = true
			}
		}
		if assign, ok := node.(*ast.AssignStmt); ok {
			for i, rhs := range assign.Rhs {
				if isMapExpr(rhs, names, rules.mapTypes, rules.mapFields) {
					lhsIndex := i
					if lhsIndex >= len(assign.Lhs) {
						lhsIndex = len(assign.Lhs) - 1
					}
					if lhsIndex >= 0 {
						if id, ok := assign.Lhs[lhsIndex].(*ast.Ident); ok {
							names[id.Name] = true
						}
					}
				}
			}
		}
		return true
	})
	for name := range rules.globals {
		if strings.HasPrefix(name, "map:") {
			names[strings.TrimPrefix(name, "map:")] = true
		}
	}
	return names
}

func realtimeStringNames(fn *ast.FuncDecl, rules packageRules) (map[string]bool, map[string]bool) {
	strings := make(map[string]bool)
	bytes := make(map[string]bool)
	for name := range rules.stringGlobals {
		strings[name] = true
	}
	collect := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, field := range fields.List {
			for _, name := range field.Names {
				if isStringType(field.Type) || isIdentIn(field.Type, rules.stringTypes) {
					strings[name.Name] = true
				}
				if isByteSliceType(field.Type) || isIdentIn(field.Type, rules.byteSliceTypes) {
					bytes[name.Name] = true
				}
			}
		}
	}
	collect(fn.Recv)
	collect(fn.Type.Params)
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if spec, ok := node.(*ast.ValueSpec); ok {
			for _, name := range spec.Names {
				if isStringType(spec.Type) || isIdentIn(spec.Type, rules.stringTypes) {
					strings[name.Name] = true
				}
				if isByteSliceType(spec.Type) || isIdentIn(spec.Type, rules.byteSliceTypes) {
					bytes[name.Name] = true
				}
			}
		}
		if assign, ok := node.(*ast.AssignStmt); ok {
			for i, rhs := range assign.Rhs {
				if i >= len(assign.Lhs) {
					break
				}
				id, ok := assign.Lhs[i].(*ast.Ident)
				if !ok {
					continue
				}
				if isStringExpr(rhs, strings) {
					strings[id.Name] = true
				}
				if isByteSliceExpr(rhs, bytes) {
					bytes[id.Name] = true
				}
			}
		}
		return true
	})
	return strings, bytes
}

func realtimeChannelNames(fn *ast.FuncDecl, rules packageRules) map[string]bool {
	names := make(map[string]bool)
	add := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, field := range fields.List {
			if isChanType(field.Type) || isIdentIn(field.Type, rules.channelTypes) {
				for _, name := range field.Names {
					names[name.Name] = true
				}
			}
		}
	}
	add(fn.Recv)
	add(fn.Type.Params)
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if spec, ok := node.(*ast.ValueSpec); ok && (isChanType(spec.Type) || isIdentIn(spec.Type, rules.channelTypes)) {
			for _, name := range spec.Names {
				names[name.Name] = true
			}
		}
		if assign, ok := node.(*ast.AssignStmt); ok {
			for i, rhs := range assign.Rhs {
				if i >= len(assign.Lhs) || !isChannelExpr(rhs, names, rules.channelTypes) {
					continue
				}
				if lhs, ok := assign.Lhs[i].(*ast.Ident); ok {
					names[lhs.Name] = true
				}
			}
		}
		return true
	})
	return names
}

func isChannelExpr(expr ast.Expr, names, channelTypes map[string]bool) bool {
	switch n := expr.(type) {
	case *ast.Ident:
		return names[n.Name] || channelTypes[n.Name]
	case *ast.CallExpr:
		id, ok := n.Fun.(*ast.Ident)
		return ok && id.Name == "make" && len(n.Args) != 0 && isChanType(n.Args[0])
	case *ast.ParenExpr:
		return isChannelExpr(n.X, names, channelTypes)
	}
	return false
}

func stringLiteral(expr ast.Expr) bool {
	literal, ok := expr.(*ast.BasicLit)
	return ok && literal.Kind == token.STRING
}

func isStringType(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "string"
}

func isByteSliceType(expr ast.Expr) bool {
	array, ok := expr.(*ast.ArrayType)
	if !ok || array.Len != nil {
		return false
	}
	id, ok := array.Elt.(*ast.Ident)
	return ok && (id.Name == "byte" || id.Name == "uint8")
}

func isByteSliceExpr(expr ast.Expr, names map[string]bool) bool {
	switch n := expr.(type) {
	case *ast.Ident:
		return names[n.Name]
	case *ast.CompositeLit:
		return isByteSliceType(n.Type)
	case *ast.CallExpr:
		if typ, ok := n.Fun.(*ast.ArrayType); ok && isByteSliceType(typ) {
			return true
		}
	}
	return false
}

func isStringExpr(expr ast.Expr, names map[string]bool) bool {
	switch n := expr.(type) {
	case *ast.BasicLit:
		return n.Kind == token.STRING
	case *ast.Ident:
		return names[n.Name]
	case *ast.ParenExpr:
		return isStringExpr(n.X, names)
	case *ast.CallExpr:
		id, ok := n.Fun.(*ast.Ident)
		return ok && id.Name == "string"
	}
	return false
}
