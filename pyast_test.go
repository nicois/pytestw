package main

import (
	"testing"
)

// TestHelloName calls greetings.Hello with a name, checking
// for a valid return value.
func TestStripComments(t *testing.T) {
	// these aren't actually comments but for our purposes
	// can be discarded
	if stripped := stripComments(`import foo ''' or not'''`); stripped != `import foo` {
		t.Fatalf("Stripped version was:\n%v\n, which is wrong", stripped)
	}

	if stripped := stripComments(`import foo """ or not"""`); stripped != `import foo` {
		t.Fatalf("Stripped version was:\n%v\n, which is wrong", stripped)
	}

	// single-line comment
	if stripped := stripComments(`import foo # or not`); stripped != `import foo` {
		t.Fatalf("Stripped version was:\n%q\n, which is wrong", stripped)
	}
	// multi-line import with comments
	if stripped := stripComments(`
from foo import (# or not
    bar,# another comment
    baz)
    `); stripped != `from foo import (
    bar,
    baz)` {
		t.Fatalf("Stripped version was:\n%q\n, which is wrong", stripped)
	}
}
