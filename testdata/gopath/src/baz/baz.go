package bar

import (
	"context"
	"foo"
)

func baz(x context.Context) {
	foo.F()
}

func alreadyHasCtxInside() {
	ctx := context.TODO()
	_ = ctx
}
