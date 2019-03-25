# go-ctxize

Sometimes it's hard to touch every source files to modify functions to context-aware, adding `ctx` to each callers.

goctxize rewrites Go source files to add `ctx context.Context` as a first argument of specified function,
with callers of the function rewritten so.

    goctxize [-var <var-spec>] <pkg>[.<name>].<func> [<pkg>...]

For example:

    // $GOPATH/src/example.com/foo/foo.go
    package foo

    func F() {
    }

    // $GOPATH/src/example.com/foo/foo_test.go
    package foo

    import "testing"

    func TestF(t *testing.T) {
        F()
    }

    // $GOPATH/src/example.com/bar/bar.go
    package bar

    import (
        "example.com/foo"
    )

    func bar() {
        foo.F()
    }

Given source above, `goctxize example.com/foo.F` produces below:

    // $GOPATH/src/example.com/foo/foo.go
    package foo

    import "context"

    func F(ctx context.Context) {
    }

    // $GOPATH/src/example.com/foo/foo_test.go
    package foo

    import (
        "context"
        "testing"
    )

    func TestF(t *testing.T) {
        ctx := context.TODO()

        F(ctx)
    }

While executing `goctxize example.com/foo.F example.com/bar` rewrites package bar too:

    // $GOPATH/src/example.com/bar/bar.go
    package bar

    import (
        "context"
        "example.com/foo"
    )

    func bar() {
        ctx := context.TODO()

        foo.F(ctx)
    }
