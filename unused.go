package unused // import "honnef.co/go/unused"

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/token"
	"go/types"
	"io"
	"os"
	"strings"

	"golang.org/x/tools/go/loader"
	"golang.org/x/tools/go/types/typeutil"
)

type graph struct {
	roots []*graphNode
	nodes map[interface{}]*graphNode
}

func (g *graph) markUsedBy(obj, usedBy interface{}) {
	objNode := g.getNode(obj)
	usedByNode := g.getNode(usedBy)
	if objNode.obj == usedByNode.obj {
		return
	}
	usedByNode.uses[objNode] = struct{}{}
}

var labelCounter = 1

func (g *graph) getNode(obj interface{}) *graphNode {
	for {
		if pt, ok := obj.(*types.Pointer); ok {
			obj = pt.Elem()
		} else {
			break
		}
	}
	_, ok := g.nodes[obj]
	if !ok {
		g.addObj(obj)
	}

	return g.nodes[obj]
}

func (g *graph) addObj(obj interface{}) {
	if pt, ok := obj.(*types.Pointer); ok {
		obj = pt.Elem()
	}
	node := &graphNode{obj: obj, uses: make(map[*graphNode]struct{}), n: labelCounter}
	g.nodes[obj] = node
	labelCounter++

	if obj, ok := obj.(*types.Struct); ok {
		n := obj.NumFields()
		for i := 0; i < n; i++ {
			field := obj.Field(i)
			g.markUsedBy(obj, field)
		}
	}
}

type graphNode struct {
	obj   interface{}
	uses  map[*graphNode]struct{}
	used  bool
	quiet bool
	n     int
}

type CheckMode int

const (
	CheckConstants CheckMode = 1 << iota
	CheckFields
	CheckFunctions
	CheckTypes
	CheckVariables

	CheckAll = CheckConstants | CheckFields | CheckFunctions | CheckTypes | CheckVariables
)

type Unused struct {
	Obj      types.Object
	Position token.Position
}

type Checker struct {
	Mode    CheckMode
	Verbose bool
	Debug   io.Writer

	graph *graph

	msCache      typeutil.MethodSetCache
	lprog        *loader.Program
	topmostCache map[*types.Scope]*types.Scope
}

func NewChecker(mode CheckMode, verbose bool) *Checker {
	return &Checker{
		Mode:    mode,
		Verbose: verbose,
		graph: &graph{
			nodes: make(map[interface{}]*graphNode),
		},
		topmostCache: make(map[*types.Scope]*types.Scope),
	}
}

func (c *Checker) checkConstants() bool { return (c.Mode & CheckConstants) > 0 }
func (c *Checker) checkFields() bool    { return (c.Mode & CheckFields) > 0 }
func (c *Checker) checkFunctions() bool { return (c.Mode & CheckFunctions) > 0 }
func (c *Checker) checkTypes() bool     { return (c.Mode & CheckTypes) > 0 }
func (c *Checker) checkVariables() bool { return (c.Mode & CheckVariables) > 0 }

func (c *Checker) markFields(typ types.Type) {
	structType, ok := typ.Underlying().(*types.Struct)
	if !ok {
		return
	}
	n := structType.NumFields()
	for i := 0; i < n; i++ {
		field := structType.Field(i)
		c.graph.markUsedBy(field, typ)
	}
}

func (c *Checker) Visit(node ast.Node) ast.Visitor {
	return nil
}

func (c *Checker) Check(paths []string) ([]Unused, error) {
	// We resolve paths manually instead of relying on go/loader so
	// that our TypeCheckFuncBodies implementation continues to work.
	goFiles, err := resolveRelative(paths)
	if err != nil {
		return nil, err
	}
	var unused []Unused

	conf := loader.Config{}
	if !c.Verbose {
		conf.TypeChecker.Error = func(error) {}
	}
	pkgs := map[string]bool{}
	for _, path := range paths {
		pkgs[path] = true
		pkgs[path+"_test"] = true
	}
	if !goFiles {
		// Only type-check the packages we directly import. Unless
		// we're specifying a package in terms of individual files,
		// because then we don't know the import path.
		conf.TypeCheckFuncBodies = func(s string) bool {
			return pkgs[s]
		}
	}
	_, err = conf.FromArgs(paths, true)
	if err != nil {
		return nil, err
	}
	c.lprog, err = conf.Load()
	if err != nil {
		return nil, err
	}

	for _, pkg := range c.lprog.InitialPackages() {
		c.processDefs(pkg)
		c.processUses(pkg)
		c.processTypes(pkg)
		c.processSelections(pkg)
		c.processAST(pkg)
	}

	for _, node := range c.graph.nodes {
		obj, ok := node.obj.(types.Object)
		if !ok {
			continue
		}
		typNode, ok := c.graph.nodes[obj.Type()]
		if !ok {
			continue
		}
		node.uses[typNode] = struct{}{}
	}

	roots := map[*graphNode]struct{}{}
	for _, root := range c.graph.roots {
		roots[root] = struct{}{}
	}
	markNodesUsed(roots)
	c.markNodesQuiet()

	if c.Debug != nil {
		c.printDebugGraph(c.Debug)
	}

	for _, node := range c.graph.nodes {
		if node.used || node.quiet {
			continue
		}
		obj, ok := node.obj.(types.Object)
		if !ok {
			continue
		}
		found := false
		if !false {
			for _, pkg := range c.lprog.InitialPackages() {
				if pkg.Pkg == obj.Pkg() {
					found = true
					break
				}
			}
		}
		if !found {
			continue
		}

		pos := c.lprog.Fset.Position(obj.Pos())
		unused = append(unused, Unused{Obj: obj, Position: pos})
	}
	return unused, nil
}

func (c *Checker) processDefs(pkg *loader.PackageInfo) {
	for _, obj := range pkg.Defs {
		if obj == nil {
			continue
		}
		c.graph.getNode(obj)

		if obj, ok := obj.(*types.TypeName); ok {
			c.graph.markUsedBy(obj.Type().Underlying(), obj.Type())
			c.graph.markUsedBy(obj.Type(), obj) // TODO is this needed?
			c.graph.markUsedBy(obj, obj.Type())
		}

		// FIXME(dominikh): we don't really want _ as roots. A _
		// variable in an otherwise unused function shouldn't mark
		// anything as used. However, _ doesn't seem to have a
		// scope associated with it.
		switch obj := obj.(type) {
		case *types.Var, *types.Const:
			if obj.Name() == "_" {
				node := c.graph.getNode(obj)
				node.quiet = true
				scope := c.topmostScope(pkg.Pkg.Scope().Innermost(obj.Pos()), pkg.Pkg)
				if scope == pkg.Pkg.Scope() {
					c.graph.roots = append(c.graph.roots, node)
				} else {
					c.graph.markUsedBy(obj, scope)
				}
			} else {
				if obj.Parent() != obj.Pkg().Scope() && obj.Parent() != nil {
					c.graph.markUsedBy(obj, c.topmostScope(obj.Parent(), obj.Pkg()))
				}
			}
		}

		if fn, ok := obj.(*types.Func); ok {
			c.graph.markUsedBy(fn, fn.Type())
		}

		if obj, ok := obj.(interface {
			Scope() *types.Scope
			Pkg() *types.Package
		}); ok {
			scope := obj.Scope()
			c.graph.markUsedBy(c.topmostScope(scope, obj.Pkg()), obj)
		}

		if c.isRoot(obj, false) {
			node := c.graph.getNode(obj)
			c.graph.roots = append(c.graph.roots, node)
			if obj, ok := obj.(*types.PkgName); ok {
				scope := obj.Pkg().Scope()
				c.graph.markUsedBy(scope, obj)
			}
		}
	}
}

func (c *Checker) processUses(pkg *loader.PackageInfo) {
	for ident, usedObj := range pkg.Uses {
		if _, ok := usedObj.(*types.PkgName); ok {
			continue
		}
		pos := ident.Pos()
		scope := pkg.Pkg.Scope().Innermost(pos)
		scope = c.topmostScope(scope, pkg.Pkg)
		if scope != pkg.Pkg.Scope() {
			c.graph.markUsedBy(usedObj, scope)
		}

		switch usedObj.(type) {
		case *types.Var, *types.Const:
			c.graph.markUsedBy(usedObj.Type(), usedObj)
		}
	}
}

func (c *Checker) processTypes(pkg *loader.PackageInfo) {
	named := map[*types.Named]*types.Pointer{}
	var interfaces []*types.Interface
	for _, tv := range pkg.Types {
		if typ, ok := tv.Type.(interface {
			Elem() types.Type
		}); ok {
			c.graph.markUsedBy(typ.Elem(), typ)
		}

		switch obj := tv.Type.(type) {
		case *types.Named:
			named[obj] = types.NewPointer(obj)
			c.graph.markUsedBy(obj, obj.Underlying())
			c.graph.markUsedBy(obj.Underlying(), obj)
		case *types.Interface:
			if obj.NumMethods() > 0 {
				interfaces = append(interfaces, obj)
			}
		}
	}

	for _, iface := range interfaces {
		for obj, objPtr := range named {
			if !types.Implements(obj, iface) && !types.Implements(objPtr, iface) {
				continue
			}
			ifaceMethods := make(map[string]struct{}, iface.NumMethods())
			n := iface.NumMethods()
			for i := 0; i < n; i++ {
				meth := iface.Method(i)
				ifaceMethods[meth.Name()] = struct{}{}
			}
			for _, obj := range []types.Type{obj, objPtr} {
				ms := c.msCache.MethodSet(obj)
				n := ms.Len()
				for i := 0; i < n; i++ {
					sel := ms.At(i)
					meth := sel.Obj().(*types.Func)
					_, found := ifaceMethods[meth.Name()]
					if !found {
						continue
					}
					c.graph.markUsedBy(meth.Type().(*types.Signature).Recv().Type(), obj) // embedded receiver
					if len(sel.Index()) > 1 {
						f := getField(obj, sel.Index()[0])
						c.graph.markUsedBy(f, obj) // embedded receiver
					}
					c.graph.markUsedBy(meth, obj)
				}
			}
		}
	}
}

func (c *Checker) processSelections(pkg *loader.PackageInfo) {
	for expr, sel := range pkg.Selections {
		if sel.Kind() != types.FieldVal {
			continue
		}
		scope := pkg.Pkg.Scope().Innermost(expr.Pos())
		c.graph.markUsedBy(expr.X, c.topmostScope(scope, pkg.Pkg))
		c.graph.markUsedBy(sel.Obj(), expr.X)
		if len(sel.Index()) > 1 {
			typ := sel.Recv()
			for _, idx := range sel.Index() {
				obj := getField(typ, idx)
				typ = obj.Type()
				c.graph.markUsedBy(obj, expr.X)
			}
		}
	}
}

func (c *Checker) processAST(pkg *loader.PackageInfo) {
	fn := func(node1 ast.Node) bool {
		if node1 == nil {
			return false
		}

		if node, ok := node1.(*ast.CompositeLit); ok {
			typ := pkg.TypeOf(node)
			if _, ok := typ.(*types.Named); ok {
				typ = typ.Underlying()
			}
			if _, ok := typ.(*types.Struct); !ok {
				return true
			}

			if isBasicStruct(node.Elts) {
				c.markFields(typ)
			}
		}

		if decl, ok := node1.(*ast.GenDecl); ok {
			for _, spec := range decl.Specs {
				spec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range spec.Names {
					if i >= len(spec.Values) {
						break
					}
					value := spec.Values[i]
					fn3 := func(node3 ast.Node) bool {
						if node3, ok := node3.(*ast.Ident); ok {
							obj := pkg.ObjectOf(node3)
							if _, ok := obj.(*types.PkgName); ok {
								return true
							}
							c.graph.markUsedBy(obj, pkg.ObjectOf(name))
						}
						return true
					}
					ast.Inspect(value, fn3)
				}
			}
		}

		expr, ok := node1.(ast.Expr)
		if !ok {
			return true
		}

		left := pkg.TypeOf(expr)
		if left == nil {
			return true
		}
		fn2 := func(node2 ast.Node) bool {
			if node2 == nil || node1 == node2 {
				return true
			}
			switch node2 := node2.(type) {
			case *ast.Ident:
				right := pkg.ObjectOf(node2)
				if right == nil {
					return true
				}
			case ast.Expr:
				right := pkg.TypeOf(expr)
				if right == nil {
					return true
				}
				c.graph.markUsedBy(right, left)
			}

			return true
		}
		ast.Inspect(node1, fn2)
		return true
	}
	for _, file := range pkg.Files {
		ast.Inspect(file, fn)
	}
}

func isBasicStruct(elts []ast.Expr) bool {
	for _, elt := range elts {
		if _, ok := elt.(*ast.KeyValueExpr); !ok {
			return true
		}
	}
	return false
}

func resolveRelative(importPaths []string) (goFiles bool, err error) {
	if len(importPaths) == 0 {
		return false, nil
	}
	if strings.HasSuffix(importPaths[0], ".go") {
		// User is specifying a package in terms of .go files, don't resolve
		return true, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return false, err
	}
	for i, path := range importPaths {
		bpkg, err := build.Import(path, wd, build.FindOnly)
		if err != nil {
			return false, fmt.Errorf("can't load package %q: %v", path, err)
		}
		importPaths[i] = bpkg.ImportPath
	}
	return false, nil
}

func isPkgScope(obj types.Object) bool {
	return obj.Parent() == obj.Pkg().Scope()
}

func isMain(obj types.Object) bool {
	if obj.Pkg().Name() != "main" {
		return false
	}
	if obj.Name() != "main" {
		return false
	}
	if !isPkgScope(obj) {
		return false
	}
	if !isFunction(obj) {
		return false
	}
	if isMethod(obj) {
		return false
	}
	return true
}

func isFunction(obj types.Object) bool {
	_, ok := obj.(*types.Func)
	return ok
}

func isMethod(obj types.Object) bool {
	if !isFunction(obj) {
		return false
	}
	return obj.(*types.Func).Type().(*types.Signature).Recv() != nil
}

func isVariable(obj types.Object) bool {
	_, ok := obj.(*types.Var)
	return ok
}

func isConstant(obj types.Object) bool {
	_, ok := obj.(*types.Const)
	return ok
}

func isType(obj types.Object) bool {
	_, ok := obj.(*types.TypeName)
	return ok
}

func isField(obj types.Object) bool {
	if obj, ok := obj.(*types.Var); ok && obj.IsField() {
		return true
	}
	return false
}

func (c *Checker) checkFlags(v interface{}) bool {
	obj, ok := v.(types.Object)
	if !ok {
		return false
	}
	if isFunction(obj) && !c.checkFunctions() {
		return false
	}
	if isVariable(obj) && !c.checkVariables() {
		return false
	}
	if isConstant(obj) && !c.checkConstants() {
		return false
	}
	if isType(obj) && !c.checkTypes() {
		return false
	}
	if isField(obj) && !c.checkFields() {
		return false
	}
	return true
}

func (c *Checker) isRoot(obj types.Object, wholeProgram bool) bool {
	// - in local mode, main, init, tests, and non-test, non-main exported are roots
	// - in global mode (not yet implemented), main, init and tests are roots

	if _, ok := obj.(*types.PkgName); ok {
		return true
	}

	if isMain(obj) || (isFunction(obj) && !isMethod(obj) && obj.Name() == "init") {
		return true
	}
	if obj.Exported() {
		// FIXME fields are only roots if the struct type would be, too
		// FIXME exported methods on unexported types aren't roots
		if (isMethod(obj) || isField(obj)) && !wholeProgram {
			return true
		}

		f := c.lprog.Fset.Position(obj.Pos()).Filename
		if strings.HasSuffix(f, "_test.go") {
			return strings.HasPrefix(obj.Name(), "Test") ||
				strings.HasPrefix(obj.Name(), "Benchmark") ||
				strings.HasPrefix(obj.Name(), "Example")
		}

		// Package-level are used, except in package main
		if isPkgScope(obj) && obj.Pkg().Name() != "main" && !wholeProgram {
			return true
		}
	}
	return false
}

func markNodesUsed(nodes map[*graphNode]struct{}) {
	for node := range nodes {
		wasUsed := node.used
		node.used = true
		if !wasUsed {
			markNodesUsed(node.uses)
		}
	}
}

func (c *Checker) markNodesQuiet() {
	for _, node := range c.graph.nodes {
		if node.used {
			continue
		}
		if obj, ok := node.obj.(types.Object); ok && !c.checkFlags(obj) {
			node.quiet = true
			continue
		}
		c.markObjQuiet(node.obj)
	}
}

func (c *Checker) markObjQuiet(obj interface{}) {
	switch obj := obj.(type) {
	case *types.Named:
		n := obj.NumMethods()
		for i := 0; i < n; i++ {
			meth := obj.Method(i)
			node := c.graph.getNode(meth)
			node.quiet = true
			c.markObjQuiet(meth.Scope())
		}
	case *types.Struct:
		n := obj.NumFields()
		for i := 0; i < n; i++ {
			field := obj.Field(i)
			c.graph.nodes[field].quiet = true
		}
	case *types.Func:
		c.markObjQuiet(obj.Scope())
	case *types.Scope:
		if obj == nil {
			return
		}
		if obj.Parent() == types.Universe {
			return
		}
		for _, name := range obj.Names() {
			v := obj.Lookup(name)
			if n, ok := c.graph.nodes[v]; ok {
				n.quiet = true
			}
		}
		n := obj.NumChildren()
		for i := 0; i < n; i++ {
			c.markObjQuiet(obj.Child(i))
		}
	}
}

func getField(typ types.Type, idx int) *types.Var {
	switch obj := typ.(type) {
	case *types.Pointer:
		return getField(obj.Elem(), idx)
	case *types.Named:
		return obj.Underlying().(*types.Struct).Field(idx)
	case *types.Struct:
		return obj.Field(idx)
	}
	return nil
}

func (c *Checker) topmostScope(scope *types.Scope, pkg *types.Package) (ret *types.Scope) {
	if top, ok := c.topmostCache[scope]; ok {
		return top
	}
	defer func() {
		c.topmostCache[scope] = ret
	}()
	if scope == pkg.Scope() {
		return scope
	}
	if scope.Parent().Parent() == pkg.Scope() {
		return scope
	}
	return c.topmostScope(scope.Parent(), pkg)
}

func (c *Checker) printDebugGraph(w io.Writer) {
	fmt.Fprintln(w, "digraph {")
	fmt.Fprintln(w, "n0 [label = roots]")
	for _, node := range c.graph.nodes {
		s := fmt.Sprintf("%s (%T)", node.obj, node.obj)
		s = strings.Replace(s, "\n", "", -1)
		s = strings.Replace(s, `"`, "", -1)
		fmt.Fprintf(w, `n%d [label = %q]`, node.n, s)
		color := "black"
		switch {
		case node.used:
			color = "green"
		case node.quiet:
			color = "orange"
		case !c.checkFlags(node.obj):
			color = "purple"
		default:
			color = "red"
		}
		fmt.Fprintf(w, "[color = %s]", color)
		fmt.Fprintln(w)
	}

	for _, node1 := range c.graph.nodes {
		for node2 := range node1.uses {
			fmt.Fprintf(w, "n%d -> n%d\n", node1.n, node2.n)
		}
	}
	for _, root := range c.graph.roots {
		fmt.Fprintf(w, "n0 -> n%d\n", root.n)
	}
	fmt.Fprintln(w, "}")
}
