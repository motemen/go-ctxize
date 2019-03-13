package bar

import (
	"context"
	"foo"
)

func baz(x context.Context) {
	foo.F()
}
