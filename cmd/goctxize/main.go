package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
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

type varSpec struct {
	name       string
	pkgPath    string
	pkgName    string
	typeName   string
	initExpr   string
	varTypeObj types.Object
}

type context struct {
	*loader.Program
	modified map[*ast.File]bool
	varSpec  varSpec
	wd       string
}

func split2(s string, b byte) (string, string, bool) {
	p := strings.LastIndexByte(s, b)
	if p == -1 {
		return "", "", false
	}

	return s[:p], s[p+1:], true
}

func parseVarSpec(s string) (varSpec, error) {
	nameType, initExpr, ok := split2(s, '=')
	if !ok {
		return varSpec{}, errors.New(`varSpec should in form of "<name> <path>.<type> = <expr>"`)
	}

	name, typeName, ok := split2(strings.TrimSpace(nameType), ' ')
	if !ok {
		return varSpec{}, errors.New(`varSpec should in form of "<name> <path>.<type> = <expr>"`)
	}

	pkgPath, typeName, ok := split2(typeName, '.')
	if !ok {
		return varSpec{}, errors.Errorf("varType should be <path>.<name>: %s", typeName)
	}

	pkgPath, pkgName, err := resolvePkgPath(pkgPath)
	if err != nil {
		return varSpec{}, err
	}

	return varSpec{
		name:     name,
		pkgName:  pkgName,
		pkgPath:  pkgPath,
		typeName: typeName,
		initExpr: initExpr,
	}, nil
}

// resolvePkgPath resolve package path to full package path
// eg. "./web" -> "path/to/pkg/web" or "github.com/pkg/path" -> "path/to/pkg/vendor/github.com/pkg/path"
func resolvePkgPath(path string) (string, string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}

	bPkg, err := build.Import(path, wd, build.ImportMode(0))
	if err != nil {
		return "", "", err
	}

	return bPkg.ImportPath, bPkg.Name, nil
}

// goctxize [-var "ctx context.Context = context.TODO()"] path/to/pkg[.Type].Func [<pkg>...]
func main() {
	log.SetPrefix("goctxize: ")

	varSpecString := flag.String("var", "ctx context.Context = context.TODO()", `inserted variable spec; must be in form of "<name> <path>.<type> = <expr>"`)
	flag.Parse()

	conf := &loader.Config{
		TypeChecker: types.Config{},
		ParserMode:  parser.ParseComments,
	}

	varSpec, err := parseVarSpec(*varSpecString)
	if err != nil {
		log.Fatalf("parsing -var: %s", err)
	}

	args := flag.Args()

	m := rxFuncSpec.FindStringSubmatch(args[0])
	if m == nil {
		usage()
	}

	pkgPath, typeName, funcName := m[1], m[2], m[3]

	conf.ImportWithTests(pkgPath)
	for _, path := range args[1:] {
		conf.ImportWithTests(path)
	}

	conf.Import(varSpec.pkgPath)

	prog, err := conf.Load()
	if err != nil {
		log.Fatal(err)
	}

	varPkgPath, _, err := resolvePkgPath(varSpec.pkgPath)
	if err != nil {
		log.Fatal(err)
	}
	if pkg, ok := prog.Imported[varPkgPath]; ok {
		varSpec.varTypeObj = pkg.Pkg.Scope().Lookup(varSpec.typeName)
	} else {
		log.Fatalf("BUG: could not resolve package: %s", varSpec.pkgPath)
	}

	pkgPath, _, err = resolvePkgPath(pkgPath)
	if err != nil {
		log.Fatal(err)
	}

	wd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	spec := funcSpec{pkgPath: pkgPath, typeName: typeName, funcName: funcName}
	c := context{
		Program:  prog,
		modified: map[*ast.File]bool{},
		varSpec:  varSpec,
		wd:       wd,
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
		filename := c.position(file.Pos()).Filename
		debugf("rewriting %s", filename)

		astutil.AddImport(prog.Fset, file, c.varSpec.pkgPath)

		var buf bytes.Buffer
		err := format.Node(&buf, prog.Fset, file)
		if err != nil {
			log.Fatal(err)
		}

		b, err := format.Source(buf.Bytes())
		if err != nil {
			log.Fatal(err)
		}

		err = ioutil.WriteFile(filename, b, 0777)
		if err != nil {
			log.Fatalf("writing %s: %s", filename, err)
		}
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: goctxize path/to/pkg[.Type].Func [<pkg>...]")
	os.Exit(2)
}

// funcSpec is a specification of fully-qualified function or method.
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

// matches take function object and checks if it matches to the specification.
// For method cases, "pkg.Typ.Meth" matches either "func (pkg.Typ) Meth()" or "func (*pkg.Type) Meth()".
func (s funcSpec) matches(funcType *types.Func) bool {
	recv := funcType.Type().(*types.Signature).Recv()
	if recv != nil {
		x := types.TypeString(recv.Type(), nil) + "." + funcType.Name()
		return strings.TrimLeft(x, "*") == s.String()
	}

	return funcType.Pkg().Path()+"."+funcType.Name() == s.String()
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

func (c *context) position(pos token.Pos) token.Position {
	p := c.Fset.Position(pos)
	p.Filename, _ = filepath.Rel(c.wd, p.Filename)
	return p
}

// rewriteCallExpr rewrites function call expression at pos to add ctx (or any other specified) to the first argument
// This function examines scope if it already has any safisfying value according to ctx's type (eg. context.Context).
func (c *context) rewriteCallExpr(scope *types.Scope, pos token.Pos) (usedExisting bool, err error) {
	_, path, _ := c.PathEnclosingInterval(pos, pos)

	var callExpr *ast.CallExpr
	for _, node := range path {
		var ok bool
		callExpr, ok = node.(*ast.CallExpr)
		if ok {
			break
		}
	}
	if callExpr == nil {
		err = errors.Errorf("BUG: %s: could not find function call expression", c.position(pos))
		return
	}

	debugf("%s: found caller", c.position(pos))

	// if varType is an interface, use satisfying variable, if any

	var varName string
	if iface, ok := c.varSpec.varTypeObj.Type().Underlying().(*types.Interface); ok {
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			if types.Implements(obj.Type(), iface) {
				varName = name
				usedExisting = true
				break
			}
		}
	}

	if varName == "" {
		varName = c.varSpec.name
	}

	callExpr.Args = append(
		[]ast.Expr{
			ast.NewIdent(varName),
		},
		callExpr.Args...,
	)

	c.markModified(callExpr.Pos())

	return
}

// ensureVar adds variable declaration to the scope at pos
func (c *context) ensureVar(pkg *loader.PackageInfo, scope *types.Scope, funcDecl *ast.FuncDecl, pos token.Pos) error {
	if scope.Lookup(c.varSpec.name) != nil {
		return nil
	}

	scope.Insert(types.NewVar(token.NoPos, pkg.Pkg, c.varSpec.name, c.varSpec.varTypeObj.Type()))

	initExpr, err := parser.ParseExpr(c.varSpec.initExpr)
	if err != nil {
		return errors.Wrapf(err, "parsing %q", c.varSpec.initExpr)
	}

	funcDecl.Body.List = append(
		[]ast.Stmt{
			&ast.AssignStmt{
				Lhs: []ast.Expr{ast.NewIdent(c.varSpec.name)},
				Rhs: []ast.Expr{initExpr},
				Tok: token.DEFINE,
			},
		},
		funcDecl.Body.List...,
	)

	c.markModified(pos)

	return nil
}

func (c *context) findScope(pkg *loader.PackageInfo, pos token.Pos) (*types.Scope, *ast.FuncDecl, error) {
	_, path, _ := c.PathEnclosingInterval(pos, pos)

	var decl *ast.FuncDecl
	for _, node := range path {
		var ok bool
		decl, ok = node.(*ast.FuncDecl)
		if ok {
			break
		}
	}
	if decl == nil {
		return nil, nil, errors.Errorf("%s: BUG: no surrounding FuncDecl found", c.Fset.Position(pos))
	}

	scope := pkg.Scopes[decl.Type]
	if scope == nil {
		return nil, nil, errors.Errorf("%s: BUG: no Scope found", c.Fset.Position(pos))
	}

	return scope, decl, nil
}

// rewriteCallers rewrites calls to functions specified by spec
// to add ctx as first argument.
func (c *context) rewriteCallers(spec funcSpec) error {
	for _, pkg := range c.Imported {
		for id, obj := range pkg.Uses {
			if f, ok := obj.(*types.Func); ok && spec.matches(f) {
				scope, funcDecl, err := c.findScope(pkg, id.Pos())
				if err != nil {
					return err
				}

				usedExisting, err := c.rewriteCallExpr(scope, id.Pos())
				if err != nil {
					return err
				}

				if !usedExisting {
					if err := c.ensureVar(pkg, scope, funcDecl, id.Pos()); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

// rewriteFuncDecls finds function declaration matching spec and modifies AST
// to make the function to have ctx (or any other specified) as the first argument.
func (c *context) rewriteFuncDecl(spec funcSpec) error {
	pkg, ok := c.Imported[spec.pkgPath]
	if !ok {
		return errors.Errorf("package %s was not found in source", spec.pkgPath)
	}

	var funcDecl *ast.FuncDecl
	for id, obj := range pkg.Info.Defs {
		if f, ok := obj.(*types.Func); ok && spec.matches(f) {
			var err error
			_, funcDecl, err = c.findScope(pkg, id.Pos())
			if err != nil {
				return err
			}
			break
		}
	}
	if funcDecl == nil {
		return errors.Errorf("could not find declaration of func %s in package %s", spec.funcName, spec.pkgPath)
	}

	debugf("%s: found definition", c.position(funcDecl.Pos()))

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

	c.markModified(funcDecl.Pos())

	return nil
}

func (c *context) markModified(pos token.Pos) {
	for _, pkg := range c.AllPackages {
		for _, file := range pkg.Files {
			if file.Pos() == token.NoPos {
				continue
			}
			f := c.Fset.File(file.Pos())
			if f.Base() <= int(pos) && int(pos) < f.Base()+f.Size() {
				c.modified[file] = true
				return
			}
		}
	}

	debugf("markModified: not found: %s", c.position(pos).Filename)
}
