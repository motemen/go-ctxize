package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"strings"

	"go/ast"
	"go/build"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/loader"

	"github.com/pkg/errors"
)

var rxFuncSpec = regexp.MustCompile(`^(.+?)(?:\.([\pL_]+))?\.([\pL_]+)$`)

const (
	varName = "ctx"
	varType = "context.Context"
	varInit = "context.TODO()"
)

type varSpec struct {
	name     string
	pkgPath  string
	pkgName  string
	typeName string
	initExpr string
}

type context struct {
	*loader.Program
	modified map[*ast.File]bool
	varSpec  varSpec
}

func split2(s string, b byte) (string, string, bool) {
	p := strings.LastIndexByte(s, b)
	if p == -1 {
		return "", "", false
	}

	return s[:p], s[p+1:], true
}

// goctxize [-var "ctx context.Context = context.TODO()"] path/to/pkg[.Type].Func [<pkg>...]
func main() {
	log.SetPrefix("goctxize: ")
	log.SetFlags(log.Lshortfile)

	varSpecString := flag.String("var", "ctx context.Context = context.TODO()", `inserted variable spec; must be in form of "<name> <path>.<type> = <expr>"`)
	flag.Parse()

	varNameType, varInit, ok := split2(*varSpecString, '=')
	if !ok {
		log.Fatalf(`-var should in form of "<name> <path>.<type> = <expr>"`)
	}

	varName, varType, ok := split2(strings.TrimSpace(varNameType), ' ')
	if !ok {
		log.Fatalf(`-var should in form of "<name> <path>.<type> = <expr>"`)
	}

	varTypePkgPath, varTypeName, ok := split2(varType, '.')
	if !ok {
		log.Fatalf("varType should be <path>.<name>: %s", varType)
	}

	varTypePkg, err := build.Import(varTypePkgPath, "", build.ImportMode(0))
	if err != nil {
		log.Fatalf("could not load package: %s", varTypePkgPath)
	}

	varTypePkgName := varTypePkg.Name

	conf := loader.Config{
		TypeChecker: types.Config{},
		ParserMode:  parser.ParseComments,
	}

	var pkgPath, typeName, funcName string

	args := flag.Args()

	m := rxFuncSpec.FindStringSubmatch(args[0])
	if m == nil {
		usage()
	}

	pkgPath, typeName, funcName = m[1], m[2], m[3]

	conf.ImportWithTests(pkgPath)
	for _, path := range args[1:] {
		conf.ImportWithTests(path)
	}

	prog, err := conf.Load()
	if err != nil {
		log.Fatal(err)
	}

	spec := funcSpec{pkgPath: pkgPath, typeName: typeName, funcName: funcName}
	c := context{
		Program:  prog,
		modified: map[*ast.File]bool{},
		varSpec: varSpec{
			name:     varName,
			pkgName:  varTypePkgName,
			pkgPath:  varTypePkgPath,
			typeName: varTypeName,
			initExpr: varInit,
		},
	}

	err = c.rewriteFuncDecl(spec)
	if err != nil {
		log.Fatal(err)
	}

	err = c.rewriteCallers(spec)
	if err != nil {
		log.Fatal(err)
	}

	for file := range c.modified {
		filename := prog.Fset.Position(file.Pos()).Filename
		debugf("rewriting %s", filename)

		astutil.AddImport(prog.Fset, file, c.varSpec.pkgPath)

		var buf bytes.Buffer
		err := format.Node(&buf, prog.Fset, file)
		if err != nil {
			log.Fatal(err)
		}

		err = ioutil.WriteFile(filename, buf.Bytes(), 0777)
		if err != nil {
			log.Fatalf("writing %s: %s", filename, err)
		}
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: goctxize path/to/pkg[.Type].Func [<pkg>...]")
	os.Exit(2)
}

type funcSpec struct {
	pkgPath  string
	typeName string
	funcName string
}

func (s funcSpec) String() string {
	if s.typeName == "" {
		return fmt.Sprintf("%s.%s", s.pkgPath, s.funcName)
	}

	return fmt.Sprintf("%s.%s.%s", s.pkgPath, s.typeName, s.funcName)
}

func (s funcSpec) matchesDef(obj types.Object) bool {
	// TODO: simply use types.Func.FullName()?

	funcType, ok := obj.(*types.Func)
	if !ok {
		return false
	}

	funcSig := funcType.Type().(*types.Signature)

	if s.typeName == "" {
		if funcSig.Recv() != nil {
			return false
		}

		return funcType.Pkg().Path() == s.pkgPath && funcType.Name() == s.funcName
	}

	if funcSig.Recv() == nil {
		return false
	}

	recvType, ok := resolvePointerType(funcSig.Recv().Type()).(*types.Named)
	if !ok {
		return false
	}

	return recvType.Obj().Pkg().Path() == s.pkgPath && recvType.Obj().Name() == s.typeName && funcType.Name() == s.funcName
}

func (s funcSpec) matchesUse(x interface{}) bool {
	if s.typeName == "" {
		funcObj, ok := x.(*types.Func)
		if !ok {
			return false
		}

		if funcObj.Pkg() == nil {
			return false
		}

		return funcObj.Pkg().Path() == s.pkgPath && funcObj.Name() == s.funcName
	}

	// use
	sel, ok := x.(*types.Selection)
	if !ok {
		return false
	}

	recvType, ok := resolvePointerType(sel.Recv()).(*types.Named)
	if !ok {
		return false
	}

	if recvType.Obj().Pkg() == nil {
		return false
	}

	return recvType.Obj().Pkg().Path() == s.pkgPath && recvType.Obj().Name() == s.typeName && sel.Obj().Name() == s.funcName
}

func resolvePointerType(typ types.Type) types.Type {
	for {
		if p, ok := typ.(*types.Pointer); ok {
			typ = p.Elem()
		}
		break
	}

	return typ
}

func debugf(format string, args ...interface{}) {
	log.Printf("debug: "+format, args...)
}

func (c *context) rewriteCallExpr(node ast.Node) error {
	prog := c.Program

	_, path, _ := prog.PathEnclosingInterval(node.Pos(), node.End())

	var callExpr *ast.CallExpr
	for _, node := range path {
		var ok bool
		callExpr, ok = node.(*ast.CallExpr)
		if ok {
			break
		}
	}

	if callExpr == nil {
		return errors.Errorf("BUG: %s: could not find function call expression", prog.Fset.Position(node.Pos()))
	}

	debugf("%s: found caller", prog.Fset.Position(node.Pos()))

	callExpr.Args = append(
		[]ast.Expr{
			ast.NewIdent(c.varSpec.name),
		},
		callExpr.Args...,
	)

	file := path[len(path)-1].(*ast.File)
	c.modified[file] = true

	return nil
}

func (c *context) ensureVar(pkg *loader.PackageInfo, node ast.Node) error {
	prog := c.Program
	_, path, _ := prog.PathEnclosingInterval(node.Pos(), node.End())

	var decl *ast.FuncDecl
	for _, node := range path {
		var ok bool
		decl, ok = node.(*ast.FuncDecl)
		if ok {
			break
		}
	}

	if decl == nil {
		return errors.Errorf("%s: BUG: no surrounding FuncDecl found", c.Fset.Position(node.Pos()))
	}

	scope := pkg.Scopes[decl.Type]
	if scope == nil {
		return errors.Errorf("%s: BUG: no Scope found", c.Fset.Position(node.Pos()))
	}

	if scope.Lookup(c.varSpec.name) != nil {
		return nil
	}

	scope.Insert(types.NewVar(token.NoPos, pkg.Pkg, c.varSpec.name, &types.Basic{ /*dummy*/ }))

	initExpr, err := parser.ParseExpr(c.varSpec.initExpr)
	if err != nil {
		return errors.Wrapf(err, "parsing %q", c.varSpec.initExpr)
	}

	decl.Body.List = append(
		[]ast.Stmt{
			&ast.AssignStmt{
				Lhs: []ast.Expr{ast.NewIdent(c.varSpec.name)},
				Rhs: []ast.Expr{initExpr},
				Tok: token.DEFINE,
			},
		},
		decl.Body.List...,
	)

	file := path[len(path)-1].(*ast.File)
	c.modified[file] = true

	return nil
}

func (c *context) rewriteCallers(spec funcSpec) error {
	prog := c.Program

	for _, pkg := range prog.Imported {
		for id, obj := range pkg.Uses {
			if spec.matchesUse(obj) {
				if err := c.rewriteCallExpr(id); err != nil {
					return err
				}

				if err := c.ensureVar(pkg, id); err != nil {
					return err
				}
			}
		}

		for expr, sel := range pkg.Selections {
			if spec.matchesUse(sel) {
				if err := c.rewriteCallExpr(expr); err != nil {
					return err
				}

				if err := c.ensureVar(pkg, expr); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (c *context) rewriteFuncDecl(spec funcSpec) error {
	prog := c.Program

	// XXX: how relative-path imports are resolved?
	// eg. ./web -> github.com/motemen/go-xxx/web

	pkg, ok := prog.Imported[spec.pkgPath]
	if !ok {
		return errors.Errorf("package %s was not found in source", spec.pkgPath)
	}

	var (
		funcDecl *ast.FuncDecl
		file     *ast.File
	)

	for id, obj := range pkg.Info.Defs {
		if !spec.matchesDef(obj) {
			continue
		}

		_, path, _ := prog.PathEnclosingInterval(id.Pos(), id.Pos())
		file = path[len(path)-1].(*ast.File)

		for _, node := range path {
			var ok bool
			if funcDecl, ok = node.(*ast.FuncDecl); ok {
				break
			}
		}
	}

	if funcDecl == nil {
		return errors.Errorf("could not find declaration of func %s in package %s", spec.funcName, spec.pkgPath)
	}

	debugf("%s: found definition", prog.Fset.Position(funcDecl.Pos()))

	funcDecl.Type.Params.List = append(
		[]*ast.Field{
			{
				Names: []*ast.Ident{
					ast.NewIdent(c.varSpec.name),
				},
				Type: &ast.SelectorExpr{
					Sel: ast.NewIdent(c.varSpec.typeName),
					X:   ast.NewIdent(c.varSpec.pkgName),
				},
			},
		},
		funcDecl.Type.Params.List...,
	)

	c.modified[file] = true

	return nil
}
