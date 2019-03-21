package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"

	"github.com/motemen/go-ctxize"
)

// goctxize [-var "ctx context.Context = context.TODO()"] path/to/pkg[.Type].Func [<pkg>...]
func main() {
	log.SetPrefix("goctxize: ")
	log.SetFlags(0)

	varSpecString := flag.String(
		"var",
		"ctx context.Context = context.TODO()",
		`inserted variable spec; must be in form of "<name> <path>.<type> = <expr>"`,
	)
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage: goctxize [flags] path/to/pkg[.Type].Func [<pkg>...]")
		flag.PrintDefaults()
	}
	flag.Parse()

	varSpec, err := ctxize.ParseVarSpec(*varSpecString)
	if err != nil {
		log.Fatalf("parsing -var: %s", err)
	}

	args := flag.Args()

	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	spec, err := ctxize.ParseFuncSpec(args[0])
	if err != nil {
		log.Fatal(err)
	}

	app := ctxize.App{
		VarSpec: varSpec,
	}

	err = app.Load(append([]string{spec.PkgPath}, args[1:]...)...)
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
