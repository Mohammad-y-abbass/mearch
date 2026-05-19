package ir

// SymbolIR represents a named declaration that is not a function.
//
// Covers: structs, interfaces, type aliases, constants, variables.
//
// Go examples:
//
//	type Scanner struct {}    → Kind: "struct",    Name: "Scanner"
//	type Reader interface {}  → Kind: "interface", Name: "Reader"
//	type MyInt = int          → Kind: "alias",     Name: "MyInt"
//	const MaxSize = 100       → Kind: "const",     Name: "MaxSize"
//	var DefaultTimeout = 30   → Kind: "var",       Name: "DefaultTimeout"
type SymbolIR struct {
	// Name is the declared name of the symbol.
	Name string

	// Qualified is the fully qualified name: "package.SymbolName".
	Qualified string

	// Kind describes what kind of symbol this is.
	// Values: "struct", "interface", "alias", "const", "var"
	Kind string

	// Visibility: "exported" or "unexported".
	Visibility string
}
