package ctxize

import (
	"bytes"
	"fmt"
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

type VarSpec struct {
	// name of the variable to insert eg. "ctx"
	Name string
	// package path of the type of the variable eg. "context"
	PkgPath string
	// name of the type of the variable eg "Context"
	TypeName string
	// initialization expression of the variable on the caller side
	InitExpr string

	pkg *build.Package

	varTypeObj types.Object
}

type App struct {
	Config *loader.Config
	*loader.Program
	modified map[*ast.File]bool
	VarSpec  *VarSpec
	Cwd      string
}

func (app *App) Init() (err error) {
	app.modified = map[*ast.File]bool{}

	app.Cwd, err = os.Getwd()
	if err != nil {
		return
	}

	if app.VarSpec == nil {
		app.VarSpec = &VarSpec{
			Name:     "ctx",
			PkgPath:  "context",
			TypeName: "Context",
			InitExpr: "context.TODO()",
		}
	}

	app.Config.Import(app.VarSpec.PkgPath)

	app.Program, err = app.Config.Load()
	if err != nil {
		return
	}

	bPkg, err := app.buildPackage(app.VarSpec.PkgPath)
	if err != nil {
		return
	}
	if pkg, ok := app.Program.Imported[bPkg.ImportPath]; ok {
		app.VarSpec.varTypeObj = pkg.Pkg.Scope().Lookup(app.VarSpec.TypeName)
	} else {
		err = errors.Errorf("BUG: could not resolve package: %s", app.VarSpec.PkgPath)
		return
	}

	app.VarSpec.pkg, err = app.buildPackage(app.VarSpec.PkgPath)
	if err != nil {
		return
	}

	return
}

func (app *App) Each(callback func(filename string, content []byte) error) error {
	fset := app.Program.Fset
	for file := range app.modified {
		filename := app.position(file.Pos()).Filename
		debugf("rewriting %s", filename)

		astutil.AddImport(fset, file, app.VarSpec.pkg.ImportPath)

		var buf bytes.Buffer
		err := format.Node(&buf, fset, file)
		if err != nil {
			return err
		}

		content, err := format.Source(buf.Bytes())
		if err != nil {
			return err
		}

		err = callback(filename, content)
		if err != nil {
			return err
		}
	}

	return nil
}

var rxVarSpec = regexp.MustCompile(`^([\pL_]+) +(\S+?)\.([\pL_]+) *= *(.+)$`)

func ParseVarSpec(s string) (*VarSpec, error) {
	m := rxVarSpec.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return nil, errors.New(`varSpec should in form of "<name> <path>.<type> = <expr>"`)
	}

	return &VarSpec{
		Name:     m[1],
		PkgPath:  m[2],
		TypeName: m[3],
		InitExpr: m[4],
	}, nil
}

func (app *App) buildPackage(path string) (*build.Package, error) {
	if app.Config.Build == nil {
		app.Config.Build = &build.Default
	}
	return app.Config.Build.Import(path, app.Cwd, build.ImportMode(0))
}

func (app *App) Rewrite(spec FuncSpec) error {
	err := app.rewriteFuncDecl(spec)
	if err != nil {
		return err
	}

	err = app.rewriteCallers(spec)
	if err != nil {
		return err
	}

	return nil
}

// FuncSpec is a specification of fully-qualified function or method.
type FuncSpec struct {
	PkgPath  string
	TypeName string
	FuncName string
}

var rxFuncSpec = regexp.MustCompile(`^(.+?)(?:\.([\pL_]+))?\.([\pL_]+)$`)

func ParseFuncSpec(s string) (spec FuncSpec, err error) {
	m := rxFuncSpec.FindStringSubmatch(s)
	if m == nil {
		err = errors.New("func spec must be in form of <pkg>[.<type>].<name>")
		return
	}

	spec.PkgPath, spec.TypeName, spec.FuncName = m[1], m[2], m[3]
	return
}

func (s FuncSpec) String() string {
	if s.TypeName == "" {
		return fmt.Sprintf("%s.%s", s.PkgPath, s.FuncName)
	}

	return fmt.Sprintf("%s.%s.%s", s.PkgPath, s.TypeName, s.FuncName)
}

// matches take function object and checks if it matches to the specification.
// For method cases, "pkg.Typ.Meth" matches either "func (pkg.Typ) Meth()" or "func (*pkg.Type) Meth()".
func (s FuncSpec) matches(funcType *types.Func) bool {
	recv := funcType.Type().(*types.Signature).Recv()
	if recv != nil {
		x := types.TypeString(recv.Type(), nil) + "." + funcType.Name()
		return strings.TrimLeft(x, "*") == s.String()
	}

	return funcType.Pkg().Path()+"."+funcType.Name() == s.String()
}

func (app *App) position(pos token.Pos) token.Position {
	p := app.Program.Fset.Position(pos)
	p.Filename, _ = filepath.Rel(app.Cwd, p.Filename)
	return p
}

// rewriteCallExpr rewrites function call expression at pos to add ctx (or any other specified) to the first argument
// This function examines scope if it already has any safisfying value according to ctx's type (eg. context.Context).
func (app *App) rewriteCallExpr(scope *types.Scope, pos token.Pos) (usedExisting bool, err error) {
	_, path, _ := app.PathEnclosingInterval(pos, pos)

	var callExpr *ast.CallExpr
	for _, node := range path {
		var ok bool
		callExpr, ok = node.(*ast.CallExpr)
		if ok {
			break
		}
	}
	if callExpr == nil {
		err = errors.Errorf("BUG: %s: could not find function call expression", app.position(pos))
		return
	}

	debugf("%s: found caller", app.position(pos))

	// if varType is an interface, use satisfying variable, if any

	var varName string
	if iface, ok := app.VarSpec.varTypeObj.Type().Underlying().(*types.Interface); ok {
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
		varName = app.VarSpec.Name
	}

	callExpr.Args = append(
		[]ast.Expr{
			ast.NewIdent(varName),
		},
		callExpr.Args...,
	)

	app.markModified(callExpr.Pos())

	return
}

// ensureVar adds variable declaration to the scope at pos
func (app *App) ensureVar(pkg *loader.PackageInfo, scope *types.Scope, funcDecl *ast.FuncDecl, pos token.Pos) error {
	if scope.Lookup(app.VarSpec.Name) != nil {
		return nil
	}

	scope.Insert(types.NewVar(token.NoPos, pkg.Pkg, app.VarSpec.Name, app.VarSpec.varTypeObj.Type()))

	initExpr, err := parser.ParseExpr(app.VarSpec.InitExpr)
	if err != nil {
		return errors.Wrapf(err, "parsing %q", app.VarSpec.InitExpr)
	}

	funcDecl.Body.List = append(
		[]ast.Stmt{
			&ast.AssignStmt{
				Lhs: []ast.Expr{ast.NewIdent(app.VarSpec.Name)},
				Rhs: []ast.Expr{initExpr},
				Tok: token.DEFINE,
			},
		},
		funcDecl.Body.List...,
	)

	app.markModified(pos)

	return nil
}

func (app *App) findScope(pkg *loader.PackageInfo, pos token.Pos) (*types.Scope, *ast.FuncDecl, error) {
	_, path, _ := app.PathEnclosingInterval(pos, pos)

	var decl *ast.FuncDecl
	for _, node := range path {
		var ok bool
		decl, ok = node.(*ast.FuncDecl)
		if ok {
			break
		}
	}
	if decl == nil {
		return nil, nil, errors.Errorf("%s: BUG: no surrounding FuncDecl found", app.Program.Fset.Position(pos))
	}

	scope := pkg.Scopes[decl.Type]
	if scope == nil {
		return nil, nil, errors.Errorf("%s: BUG: no Scope found", app.Program.Fset.Position(pos))
	}

	return scope, decl, nil
}

// rewriteCallers rewrites calls to functions specified by spec
// to add ctx as first argument.
func (app *App) rewriteCallers(spec FuncSpec) error {
	for _, pkg := range app.Imported {
		for id, obj := range pkg.Uses {
			if f, ok := obj.(*types.Func); ok && spec.matches(f) {
				scope, funcDecl, err := app.findScope(pkg, id.Pos())
				if err != nil {
					return err
				}

				usedExisting, err := app.rewriteCallExpr(scope, id.Pos())
				if err != nil {
					return err
				}

				if !usedExisting {
					if err := app.ensureVar(pkg, scope, funcDecl, id.Pos()); err != nil {
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
func (app *App) rewriteFuncDecl(spec FuncSpec) error {
	pkg, ok := app.Imported[spec.PkgPath]
	if !ok {
		return errors.Errorf("package %s was not found in source", spec.PkgPath)
	}

	var funcDecl *ast.FuncDecl
	for id, obj := range pkg.Info.Defs {
		if f, ok := obj.(*types.Func); ok && spec.matches(f) {
			var err error
			_, funcDecl, err = app.findScope(pkg, id.Pos())
			if err != nil {
				return err
			}
			break
		}
	}
	if funcDecl == nil {
		return errors.Errorf("could not find declaration of func %s in package %s", spec.FuncName, spec.PkgPath)
	}

	debugf("%s: found definition", app.position(funcDecl.Pos()))

	funcDecl.Type.Params.List = append(
		[]*ast.Field{
			{
				Names: []*ast.Ident{
					ast.NewIdent(app.VarSpec.Name),
				},
				Type: &ast.SelectorExpr{
					Sel: ast.NewIdent(app.VarSpec.TypeName),
					X:   ast.NewIdent(app.VarSpec.pkg.Name),
				},
			},
		},
		funcDecl.Type.Params.List...,
	)

	app.markModified(funcDecl.Pos())

	return nil
}

func (app *App) markModified(pos token.Pos) {
	for _, pkg := range app.AllPackages {
		for _, file := range pkg.Files {
			if file.Pos() == token.NoPos {
				continue
			}
			f := app.Program.Fset.File(file.Pos())
			if f.Base() <= int(pos) && int(pos) < f.Base()+f.Size() {
				app.modified[file] = true
				return
			}
		}
	}

	debugf("markModified: not found: %s", app.position(pos).Filename)
}

func debugf(format string, args ...interface{}) {
	log.Printf("debug: "+format, args...)
}
