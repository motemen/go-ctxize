package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"

	"go/parser"

	"golang.org/x/tools/go/loader"

	"github.com/motemen/go-ctxize"
)

// goctxize [-var "ctx context.Context = context.TODO()"] path/to/pkg[.Type].Func [<pkg>...]
func main() {
	log.SetPrefix("goctxize: ")

	varSpecString := flag.String(
		"var",
		"ctx context.Context = context.TODO()",
		`inserted variable spec; must be in form of "<name> <path>.<type> = <expr>"`,
	)
	flag.Parse()

	varSpec, err := ctxize.ParseVarSpec(*varSpecString)
	if err != nil {
		log.Fatalf("parsing -var: %s", err)
	}

	args := flag.Args()

	spec, err := ctxize.ParseFuncSpec(args[0])
	if err != nil {
		log.Fatal(err)
	}

	app := ctxize.App{
		Config: &loader.Config{
			ParserMode: parser.ParseComments,
		},
		VarSpec: varSpec,
	}

	app.Config.ImportWithTests(spec.PkgPath)
	for _, path := range args[1:] {
		app.Config.ImportWithTests(path)
	}

	err = app.Init()
	if err != nil {
		log.Fatal(err)
	}

	err = app.Rewrite(spec)
	if err != nil {
		log.Fatal(err)
	}

	err = app.Each(func(filename string, content []byte) error {
		return ioutil.WriteFile(filename, content, 0777)
	})
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: goctxize path/to/pkg[.Type].Func [<pkg>...]")
	os.Exit(2)
}

func debugf(format string, args ...interface{}) {
	log.Printf("debug: "+format, args...)
}
