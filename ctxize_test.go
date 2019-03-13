package ctxize

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go/build"

	"golang.org/x/tools/go/loader"
)

func TestRewrite(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	testdataContext := build.Default
	testdataContext.GOPATH = filepath.Join(cwd, "testdata", "gopath")

	app := &App{
		Config: &loader.Config{
			Build: &testdataContext,
		},
	}

	app.Config.ImportWithTests("foo")
	app.Config.ImportWithTests("bar")
	app.Config.ImportWithTests("baz")

	err = app.Init()
	if err != nil {
		t.Fatal(err)
	}

	err = app.Rewrite(FuncSpec{FuncName: "F", PkgPath: "foo"})
	if err != nil {
		t.Fatal(err)
	}

	expects := map[string][]string{
		"foo.go":      {"func F(ctx context.Context)"},
		"bar.go":      {"ctx := context.TODO()", "foo.F(ctx)"},
		"baz.go":      {"foo.F(x)"},
		"foo_test.go": {"F(ctx)"},
	}
	testFiles(t, app, expects)
}
func TestRewriteWithVarSpec(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	testdataContext := build.Default
	testdataContext.GOPATH = filepath.Join(cwd, "testdata", "gopath")

	app := &App{
		Config: &loader.Config{
			Build: &testdataContext,
		},
		VarSpec: &VarSpec{
			Name:     "t",
			PkgPath:  "go-qux",
			TypeName: "T",
			InitExpr: "0",
		},
	}

	app.Config.ImportWithTests("go-quux")

	err = app.Init()
	if err != nil {
		t.Fatal(err)
	}

	err = app.Rewrite(FuncSpec{FuncName: "F", PkgPath: "go-quux"})
	if err != nil {
		t.Fatal(err)
	}

	expects := map[string][]string{
		"quux.go": {
			`import "go-qux"`,
			"func F(t qux.T, n int)",
			"t := 0",
		},
	}
	testFiles(t, app, expects)
}

func testFiles(t *testing.T, app *App, expects map[string][]string) {
	err := app.Each(func(filename string, content []byte) error {
		t.Log(filename, string(content))
		name := filepath.Base(filename)
		if lines, ok := expects[name]; ok {
			for _, line := range lines {
				if !strings.Contains(string(content), line) {
					t.Errorf("file %s should contain %q but got:\n%s", filename, line, string(content))
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
