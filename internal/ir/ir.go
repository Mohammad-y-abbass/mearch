// Package ir defines the Intermediate Representation types for Mearch.
//
// The IR is the semantic foundation of the entire engine.
// Everything above the parser layer operates on IR, never on raw ASTs.
//
// Core philosophy:
//
//	IR represents WHAT code means, not HOW it is written.
//
// A FileIR is the unit of indexing. One source file produces one FileIR.
// The graph builder consumes FileIRs and converts them into nodes and edges.
//
// IR types are intentionally simple value structs — no methods, no behaviour.
// They are pure data. Validation and construction logic lives in the extractor.
package ir

// FileIR is the complete semantic representation of a single source file.
//
// This is the top-level output of the IR extraction phase.
// The graph builder consumes FileIRs directly.
type FileIR struct {
	// Path is the absolute path of the source file.
	Path string

	// Language is the source language (e.g. "go", "typescript").
	Language string

	// Package is the package or module name declared in this file.
	// For Go: the identifier after the `package` keyword.
	Package string

	// Imports are all import declarations in this file.
	Imports []ImportIR

	// Functions are all top-level function declarations.
	// Does NOT include methods — those are in the type they belong to,
	// accessible via Symbols.
	Functions []FunctionIR

	// Symbols are all named type-level declarations:
	// structs, interfaces, type aliases, constants, variables.
	Symbols []SymbolIR

	// Calls are all outbound call expressions detected in this file.
	// Used to build the call graph.
	Calls []CallIR

	// Edges are explicit semantic relationships extracted from this file.
	// The graph builder converts these directly into graph edges.
	Edges []EdgeIR
}

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

// FunctionIR represents a function or method declaration.
//
// Go examples:
//
//	func Foo() {}                       → Name: "Foo",  Receiver: ""
//	func (s *Scanner) Scan() {}        → Name: "Scan", Receiver: "*Scanner"
type FunctionIR struct {
	// Name is the unqualified function or method name.
	Name string

	// Qualified is the fully qualified name including package and receiver.
	// Used as a stable, collision-free identifier in the graph.
	//
	// Format for functions: "package.FuncName"
	// Format for methods:   "package.ReceiverType.MethodName"
	//
	// Example: "scanner.Scanner.Scan"
	Qualified string

	// Receiver is the method receiver type, if this is a method.
	// Empty for plain functions.
	// Stored without pointer indicator for normalisation:
	// both "(s *Scanner)" and "(s Scanner)" produce Receiver: "Scanner".
	Receiver string

	// Visibility indicates whether the symbol is exported.
	// "exported"   — name starts with uppercase
	// "unexported" — name starts with lowercase
	Visibility string
}

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

// CallIR represents a single outbound call expression.
//
// Go examples:
//
//	fmt.Println("hello")  → Caller: "main.main", Target: "fmt.Println", Kind: "external"
//	s.Scan()              → Caller: "main.main", Target: "Scan",         Kind: "internal"
//	len(x)                → Caller: "main.main", Target: "len",          Kind: "builtin"
type CallIR struct {
	// Caller is the qualified name of the function making the call.
	// Format: "package.FuncName" or "package.ReceiverType.MethodName"
	Caller string

	// Target is the name of the function or method being called.
	// May be qualified ("fmt.Println") or unqualified ("Scan") depending
	// on how it appears in source.
	Target string

	// Kind classifies the call target.
	Kind CallKind
}

// CallKind classifies the target of a call expression.
type CallKind string

const (
	// CallKindBuiltin is a call to a Go builtin: len, cap, make, new, etc.
	CallKindBuiltin CallKind = "builtin"

	// CallKindExternal is a call to a symbol from another package.
	// Identified by a qualified name with a dot: pkg.Symbol
	CallKindExternal CallKind = "external"

	// CallKindInternal is a call to a symbol within the same package.
	CallKindInternal CallKind = "internal"

	// CallKindMethod is a method call on a receiver: receiver.Method()
	CallKindMethod CallKind = "method"
)

// EdgeIR represents an explicit semantic relationship between two entities.
//
// Edges are the raw material for the graph builder.
// Every relationship in the codebase that Mearch cares about is
// eventually expressed as one or more EdgeIRs.
//
// Examples:
//
//	main.go imports fmt          → From: "main.go",      To: "fmt",         Kind: EdgeKindImport
//	Scanner defines Scan         → From: "Scanner",      To: "Scan",        Kind: EdgeKindDefine
//	main calls fmt.Println       → From: "main.main",    To: "fmt.Println", Kind: EdgeKindCall
type EdgeIR struct {
	// From is the qualified name of the source entity.
	From string

	// To is the qualified name of the target entity.
	To string

	// Kind describes the semantic relationship.
	Kind EdgeKind
}

// EdgeKind describes the type of relationship an EdgeIR represents.
type EdgeKind string

const (
	// EdgeKindImport: From file imports To package.
	EdgeKindImport EdgeKind = "import"

	// EdgeKindCall: From function calls To function.
	EdgeKindCall EdgeKind = "call"

	// EdgeKindDefine: From type defines To method or field.
	EdgeKindDefine EdgeKind = "define"

	// EdgeKindUse: From symbol uses To type.
	EdgeKindUse EdgeKind = "use"

	// EdgeKindInherit: From type inherits/embeds To type.
	EdgeKindInherit EdgeKind = "inherit"

	// EdgeKindImplement: From type implements To interface.
	EdgeKindImplement EdgeKind = "implement"

	// EdgeKindCompose: From struct composes To struct via embedding.
	EdgeKindCompose EdgeKind = "compose"
)
