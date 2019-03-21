# go-ctxize

Sometimes it's hard to touch every source files to modify functions to context-aware, adding `ctx` to each callers.

goctxize rewrites Go source files to add `ctx context.Context` as a first argument of specified function,
with callers of the function rewritten so.

    goctxize [-var <var-spec>] <pkg>[.<name>].<func> [<pkg>...]

For example:

    // $GOPATH/src/foo/foo.go
    package foo

    func F() {
    }

    // $GOPATH/src/bar/bar.go
    package bar

    func bar() {
        foo.F()
    }

Given source above, `goctxize foo.F` produces below:

    // $GOPATH/src/foo/foo.go
    package foo

    import "context"

    func F(ctx context.Context) {
    }

While executing `goctxize foo.F bar` rewrites package bar too:

    // $GOPATH/src/bar/bar.go
    package bar

    import (
        "context"
        "foo"
    )

    func bar() {
        ctx := context.TODO()

        foo.F(ctx)
    }
