package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"regexp"

	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/loader"

	"github.com/pkg/errors"
)

var rxFuncSpec = regexp.MustCompile(`^(.+?)(?:\.([A-Za-z0-9_]+))?\.([A-Za-z0-9_]+)$`) // FIXME ident characters

type context struct {
	*loader.Program
	modified map[*ast.File]bool
}

// goctxize path/to/pkg.Func [<pkg>...]
func main() {
	log.SetPrefix("goctxize: ")
	log.SetFlags(log.Lshortfile)

	conf := loader.Config{
		TypeChecker: types.Config{},
		ParserMode:  parser.ParseComments,
	}

	var pkgPath, typeName, funcName string

	m := rxFuncSpec.FindStringSubmatch(os.Args[1])
	if m == nil {
		usage()
	}

	pkgPath, typeName, funcName = m[1], m[2], m[3]

	conf.Import(pkgPath)
	for _, path := range os.Args[2:] {
		conf.Import(path)
	}

	prog, err := conf.Load()
	if err != nil {
		log.Fatal(err)
	}

	spec := funcSpec{pkgPath: pkgPath, typeName: typeName, funcName: funcName}
	c := context{
		Program:  prog,
		modified: map[*ast.File]bool{},
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

		astutil.AddImport(prog.Fset, file, "context")

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
	fmt.Fprintln(os.Stderr, "usage: goctxize path/to/pkg[.Type].Func [path/to/pkg.Type]")
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
			ast.NewIdent("ctx"),
		},
		callExpr.Args...,
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

				if obj.Parent().Lookup("ctx") != nil {
					continue
				}

				_, path, _ := prog.PathEnclosingInterval(id.Pos(), id.Pos())

				var decl *ast.FuncDecl
				for _, node := range path {
					var ok bool
					decl, ok = node.(*ast.FuncDecl)
					if ok {
						break
					}
				}

				if decl == nil {
					return errors.Errorf("%s: BUG: no surrounding FuncDecl found", c.Fset.Position(id.Pos()))
				}

				scope := pkg.Scopes[decl.Type]
				if scope == nil {
					return errors.Errorf("%s: BUG: no Scope found", c.Fset.Position(id.Pos()))
				}

				if scope.Lookup("ctx") != nil {
					continue
				}

				initExpr, err := parser.ParseExpr("context.TODO()")
				if err != nil {
					return err
				}

				decl.Body.List = append(
					[]ast.Stmt{
						&ast.AssignStmt{
							Lhs: []ast.Expr{ast.NewIdent("ctx")},
							Rhs: []ast.Expr{initExpr},
							Tok: token.DEFINE,
						},
					},
					decl.Body.List...,
				)

				file := path[len(path)-1].(*ast.File)
				c.modified[file] = true
			}
		}

		for expr, sel := range pkg.Selections {
			if spec.matchesUse(sel) {
				if err := c.rewriteCallExpr(expr); err != nil {
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
					ast.NewIdent("ctx"),
				},
				Type: &ast.SelectorExpr{
					Sel: ast.NewIdent("Context"),
					X:   ast.NewIdent("context"),
				},
			},
		},
		funcDecl.Type.Params.List...,
	)

	c.modified[file] = true

	return nil
}
