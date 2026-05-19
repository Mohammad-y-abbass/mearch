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
