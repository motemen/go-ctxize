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
	"go/format"
	"go/parser"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"

	"github.com/pkg/errors"
)

// VarSpec is a specification of the variable to be prepended to the arguments
// of the func specified by FuncSpec.
type VarSpec struct {
	// name of the variable to insert eg. "ctx"
	Name string
	// package path of the type of the variable eg. "context"
	PkgPath string
	// name of the type of the variable eg "Context"
	TypeName string
	// initialization expression of the variable on the caller side
	InitExpr string

	// resolved package information pointed by PkgPath
	pkg *packages.Package

	// type object of PkgPath.TypeName
	varTypeObj types.Object
}

// App is an entry point of go-ctxize
type App struct {
	Config   *packages.Config
	VarSpec  *VarSpec
	Cwd      string
	modified map[*ast.File]bool
	pkgs     []*packages.Package
}

// Init prepares required objects and start loading packages given.
func (app *App) Init(pkgPaths ...string) (err error) {
	if app.Config == nil {
		app.Config = &packages.Config{
			Tests: true,
		}
	}

	app.Config.Mode = packages.LoadAllSyntax

	if app.Config.Fset == nil {
		app.Config.Fset = token.NewFileSet()
	}

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

	app.pkgs, err = packages.Load(app.Config, append([]string{app.VarSpec.PkgPath}, pkgPaths...)...)
	if err != nil {
		return
	}

	varPkg, err := app.resolvePackage(app.VarSpec.PkgPath)
	if err != nil {
		return
	}

	app.VarSpec.pkg = varPkg
	app.VarSpec.varTypeObj = varPkg.Types.Scope().Lookup(app.VarSpec.TypeName)
	if app.VarSpec.varTypeObj == nil {
		err = errors.Errorf("cannot find type %s in package %s", app.VarSpec.TypeName, varPkg.PkgPath)
		return
	}

	return
}

func (app *App) resolvePackage(path string) (*packages.Package, error) {
	var conf = *app.Config // copy
	conf.Mode = packages.LoadFiles
	conf.Tests = false

	pp, err := packages.Load(&conf, path)
	if err != nil {
		return nil, err
	}
	for _, pkg := range app.pkgs {
		if pkg.ID == pp[0].ID {
			return pkg, nil
		}
	}

	return nil, errors.Errorf("cannot resolve package %q", path)
}

// Each visits all files rewritten along with their contents.
func (app *App) Each(callback func(filename string, content []byte) error) error {
	fset := app.Config.Fset
	for file := range app.modified {
		filename := app.position(file.Pos()).Filename

		astutil.AddImport(fset, file, app.VarSpec.pkg.PkgPath)

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

// ParseVarSpec parses var spec string.
// Spec string must be "<name> <path>.<type> = <expr>",
// eg. "ctx context.Context = context.TODO()"
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

/*
func (app *App) findPackage(path string) (*build.Package, error) {
	buildContext := app.Config.Build
	if buildContext == nil {
		buildContext = &build.Default
	}

	findPackage := app.Config.FindPackage
	if findPackage == nil {
		findPackage = (*build.Context).Import
	}

	return findPackage(buildContext, path, app.Cwd, build.ImportMode(0))
}
*/

// Rewrite visits all packages which are Import()ed to
// prepend variable specified by VarSpec to functions and calls
// specified by spec.
// Before calling this method, Init() must be called.
func (app *App) Rewrite(spec FuncSpec) error {
	var err error
	spec.pkg, err = app.resolvePackage(spec.PkgPath)
	if err != nil {
		return err
	}

	err = app.rewriteFuncDecl(spec)
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

	// resolved package information pointed by PkgPath
	pkg *packages.Package
}

var rxFuncSpec = regexp.MustCompile(`^(.+?)(?:\.([\pL_]+))?\.([\pL_]+)$`)

// ParseFuncSpec parses a string s to produce FuncSpec.
// s must be in form of <pkg>[.<type>].<name>.
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
		return fmt.Sprintf("%s.%s", s.pkg.PkgPath, s.FuncName)
	}

	return fmt.Sprintf("%s.%s.%s", s.pkg.PkgPath, s.TypeName, s.FuncName)
}

// matches takes function object and checks if it matches to the specification.
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
	p := app.Config.Fset.Position(pos)
	p.Filename, _ = filepath.Rel(app.Cwd, p.Filename)
	return p
}

func (app *App) pathEnclosingInterval(start, end token.Pos) ([]ast.Node, bool) {
	for _, pkg := range app.pkgs {
		for _, file := range pkg.Syntax {
			f := app.Config.Fset.File(file.Pos())
			if f.Base() <= int(start) && int(start) < f.Base()+f.Size() {
				return astutil.PathEnclosingInterval(file, start, end)
			}
		}
	}

	return nil, false
}

// rewriteCallExpr rewrites function call expression at pos to add ctx (or any other specified) to the first argument
// This function examines scope if it already has any safisfying value according to ctx's type (eg. context.Context).
func (app *App) rewriteCallExpr(scope *types.Scope, pos token.Pos) (usedExisting bool, err error) {
	path, _ := app.pathEnclosingInterval(pos, pos)

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
func (app *App) ensureVar(pkg *packages.Package, scope *types.Scope, funcDecl *ast.FuncDecl, pos token.Pos) error {
	if scope.Lookup(app.VarSpec.Name) != nil {
		return nil
	}

	scope.Insert(types.NewVar(token.NoPos, pkg.Types, app.VarSpec.Name, app.VarSpec.varTypeObj.Type()))

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

func (app *App) findScope(pkg *packages.Package, pos token.Pos) (*types.Scope, *ast.FuncDecl, error) {
	path, _ := app.pathEnclosingInterval(pos, pos)

	var decl *ast.FuncDecl
	for _, node := range path {
		var ok bool
		decl, ok = node.(*ast.FuncDecl)
		if ok {
			break
		}
	}
	if decl == nil {
		return nil, nil, errors.Errorf("%s: BUG: no surrounding FuncDecl found", app.Config.Fset.Position(pos))
	}

	scope := pkg.TypesInfo.Scopes[decl.Type]
	if scope == nil {
		return nil, nil, errors.Errorf("%s: BUG: no Scope found", app.Config.Fset.Position(pos))
	}

	return scope, decl, nil
}

// rewriteCallers rewrites calls to functions specified by spec
// to add ctx as first argument.
func (app *App) rewriteCallers(spec FuncSpec) error {
	for _, pkg := range app.pkgs {
		for id, obj := range pkg.TypesInfo.Uses {
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
	var funcDecl *ast.FuncDecl
	for id, obj := range spec.pkg.TypesInfo.Defs {
		if f, ok := obj.(*types.Func); ok && spec.matches(f) {
			var err error
			_, funcDecl, err = app.findScope(spec.pkg, id.Pos())
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
	for _, pkg := range app.pkgs {
		for _, file := range pkg.Syntax {
			if file.Pos() == token.NoPos {
				continue
			}
			f := app.Config.Fset.File(file.Pos())
			if f.Base() <= int(pos) && int(pos) < f.Base()+f.Size() {
				app.modified[file] = true
				return
			}
		}
	}

	debugf("markModified: not found: %s", app.position(pos).Filename)
}

var Debug bool = true

func debugf(format string, args ...interface{}) {
	if Debug {
		log.Printf("debug: "+format, args...)
	}
}
