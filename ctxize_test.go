package ctxize

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages/packagestest"
)

var testdata = []packagestest.Module{
	testPackage("example.com/foo"),
	testPackage("example.com/bar"),
	testPackage("example.com/baz"),
	testPackage("example.com/go-qux"),
	testPackage("example.com/go-quux"),
}

func testPackage(pkgPath string) packagestest.Module {
	return packagestest.Module{
		Name:  pkgPath,
		Files: packagestest.MustCopyFileTree(filepath.Join("testdata", filepath.FromSlash(pkgPath))),
	}
}

func TestRewrite(t *testing.T) {
	packagestest.TestAll(t, testRewrite)
}

func testRewrite(t *testing.T, exporter packagestest.Exporter) {
	exported := packagestest.Export(t, exporter, testdata)
	defer exported.Cleanup()

	app := &App{
		Config: exported.Config,
	}

	err := app.Load("example.com/foo", "example.com/bar", "example.com/baz")
	if err != nil {
		t.Fatal(err)
	}

	err = app.Rewrite(FuncSpec{FuncName: "F", PkgPath: "example.com/foo"})
	if err != nil {
		t.Fatal(err)
	}

	expects := map[string][]string{
		"foo.go":      {"func F(ctx context.Context)"},
		"bar.go":      {"ctx := context.TODO()", "foo.F(ctx)"},
		"baz.go":      {"foo.F(x)"},
		"foo_test.go": {"F(ctx)"},
	}
	testFileContents(t, app, expects)
}

func TestRewrite_withVarSpec(t *testing.T) {
	packagestest.TestAll(t, testRewrite_withVarSpec)
}

func testRewrite_withVarSpec(t *testing.T, exporter packagestest.Exporter) {
	exported := packagestest.Export(t, exporter, testdata)
	defer exported.Cleanup()

	app := &App{
		Config: exported.Config,
		VarSpec: &VarSpec{
			Name:     "t",
			PkgPath:  "example.com/go-qux",
			TypeName: "T",
			InitExpr: "0",
		},
	}

	err := app.Load("example.com/go-quux")
	if err != nil {
		t.Fatal(err)
	}

	err = app.Rewrite(FuncSpec{FuncName: "F", PkgPath: "example.com/go-quux"})
	if err != nil {
		t.Fatal(err)
	}

	expects := map[string][]string{
		"quux.go": {
			`import "example.com/go-qux"`,
			"func F(t qux.T, n int)",
			"t := 0",
		},
	}
	testFileContents(t, app, expects)
}

func testFileContents(t *testing.T, app *App, expects map[string][]string) {
	err := app.Each(func(filename string, content []byte) error {
		t.Log(filename, string(content))
		name := filepath.Base(filename)
		if lines, ok := expects[name]; ok {
			for _, line := range lines {
				shouldExist := true
				if line[0] == '!' {
					shouldExist = false
					line = line[1:]
				}
				if strings.Contains(string(content), line) != shouldExist {
					t.Errorf("'file %s contains %q' must be %v but got:\n%s", filename, line, shouldExist, string(content))
				}
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestParseVarSpec(t *testing.T) {
	tests := []struct {
		spec     string
		expected *VarSpec
	}{
		{
			spec: "ctx context.Context = context.TODO()",
			expected: &VarSpec{
				Name:     "ctx",
				PkgPath:  "context",
				TypeName: "Context",
				InitExpr: "context.TODO()",
			},
		},
		{
			spec: "v path/to/pkg.T = f()",
			expected: &VarSpec{
				Name:     "v",
				PkgPath:  "path/to/pkg",
				TypeName: "T",
				InitExpr: "f()",
			},
		},
	}

	for _, test := range tests {
		varSpec, err := ParseVarSpec(test.spec)
		if err != nil {
			t.Errorf("ParseVarSpec: %q: %s", test.spec, err)
		}

		if !reflect.DeepEqual(varSpec, test.expected) {
			t.Errorf("expected %+v but got %+v", test.expected, varSpec)
		}
	}
}

func TestRewrite_RemoveCtxTODO(t *testing.T) {
	exported := packagestest.Export(t, packagestest.Modules, testdata)
	defer exported.Cleanup()

	app := &App{
		Config: exported.Config,
	}

	err := app.Load("example.com/foo", "example.com/baz")
	if err != nil {
		t.Fatal(err)
	}

	err = app.Rewrite(FuncSpec{FuncName: "alreadyHasCtxInside", PkgPath: "example.com/baz"})
	if err != nil {
		t.Fatal(err)
	}

	expects := map[string][]string{
		"baz.go": {"!context.TODO()"},
	}
	testFileContents(t, app, expects)
}
