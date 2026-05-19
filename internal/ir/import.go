package ir

// ImportIR represents a single import declaration.
//
// Go examples:
//
//	import "fmt"                        → Path: "fmt",  Alias: ""
//	import f "fmt"                      → Path: "fmt",  Alias: "f"
//	import . "fmt"                      → Path: "fmt",  Alias: "."
//	import _ "fmt"                      → Path: "fmt",  Alias: "_"
type ImportIR struct {
	// Path is the import path string (without quotes).
	// e.g. "fmt", "github.com/yourorg/mearch/internal/parser"
	Path string

	// Alias is the local name given to the import, if any.
	// Empty string means the package's declared name is used.
	// "." means the package's symbols are injected into the current scope.
	// "_" means the package is imported for side effects only.
	Alias string
}
