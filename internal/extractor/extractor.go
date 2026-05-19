// Package extractor implements IR extraction for Mearch.
//
// The extractor sits between the parser and the graph builder:
//
//	Parser → [Extractor] → Graph Builder
//
// Responsibility: walk a Tree-sitter syntax tree and extract semantic
// meaning into a FileIR. The extractor knows about language-specific
// grammar details. The graph builder does not.
//
// This file implements Go extraction only.
// Each language gets its own extractor file — they share the IR types
// but nothing else.
//
// Extraction strategy:
// Mearch uses Tree-sitter *queries* rather than manual recursive AST walkers.
// Queries are declarative, easier to read, and simpler to extend.
// Each semantic concept (imports, functions, calls, etc.) has its own query.
package extractor

import (
	"path/filepath"
	"strings"

	"github.com/mohamamd-y-abbass/mearch/internal/ir"
	"github.com/mohamamd-y-abbass/mearch/internal/parser"
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
)

// Go builtin functions — used to classify CallIRs.
// Checked during call extraction to set CallKindBuiltin.
var goBuiltins = map[string]bool{
	"len": true, "cap": true, "make": true, "new": true,
	"append": true, "copy": true, "delete": true, "close": true,
	"panic": true, "recover": true, "print": true, "println": true,
	"real": true, "imag": true, "complex": true, "clear": true,
	"min": true, "max": true,
}

// GoExtractor extracts a FileIR from a parsed Go source file.
//
// GoExtractor is stateless — all state is local to each Extract() call.
// Safe to reuse across files. Safe to use concurrently from multiple
// goroutines (each call operates on its own ParseResult).
type GoExtractor struct {
	// tsLang is the Tree-sitter language instance used for query compilation.
	// Compiled once at construction and reused across Extract() calls.
	tsLang *tree_sitter.Language
}

// NewGoExtractor constructs a GoExtractor.
//
// Returns an error if the Tree-sitter language cannot be initialised —
// this should never happen in practice but is checked for safety.
func NewGoExtractor() *GoExtractor {
	return &GoExtractor{
		tsLang: tree_sitter.NewLanguage(tree_sitter_go.Language()),
	}
}

// Extract produces a FileIR from a parsed Go file.
//
// result must be a ParseResult from a .go file. Passing a result from
// another language produces undefined behaviour.
//
// Extract never returns a nil FileIR. If the file has parse errors,
// extraction continues for the nodes that did parse correctly —
// partial indexing is always preferred over no indexing.
func (e *GoExtractor) Extract(result *parser.ParseResult) (*ir.FileIR, error) {
	file := &ir.FileIR{
		Path:     result.Path,
		Language: "go",
	}

	root := result.RootNode()
	src := result.Source

	// Extract each semantic category independently.
	// Order matters for Edges — functions and symbols must be extracted
	// before edges so qualified names are available.
	file.Package = e.extractPackage(root, src)
	file.Imports = e.extractImports(root, src)
	file.Functions = e.extractFunctions(root, src, file.Package)
	file.Symbols = e.extractSymbols(root, src, file.Package)
	file.Calls = e.extractCalls(root, src, file.Package, file.Functions)
	file.Edges = e.buildEdges(file)

	return file, nil
}

// --- Package extraction ---

// extractPackage returns the package name declared in this file.
//
// Query targets:
//
//	package main   →  "main"
//	package scanner →  "scanner"
func (e *GoExtractor) extractPackage(root *tree_sitter.Node, src []byte) string {
	query, err := tree_sitter.NewQuery(e.tsLang, `
		(package_clause
			(package_identifier) @pkg)
	`)
	if err != nil {
		return ""
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		for _, capture := range match.Captures {
			return string(capture.Node.Utf8Text(src))
		}
	}
	return ""
}

// --- Import extraction ---

// extractImports returns all import declarations in the file.
//
// Handles both single and grouped imports:
//
//	import "fmt"
//	import (
//	    "os"
//	    f "fmt"
//	    _ "net/http"
//	)
func (e *GoExtractor) extractImports(root *tree_sitter.Node, src []byte) []ir.ImportIR {
	// Two captures per import spec:
	// @alias — optional local name (may not be present)
	// @path  — the import path string (always present)
	//
	// Tree-sitter will only include @alias in captures when it exists,
	// so we need to handle both 1-capture and 2-capture matches.
	query, err := tree_sitter.NewQuery(e.tsLang, `
		(import_spec
			name: (package_identifier)? @alias
			path: (interpreted_string_literal) @path)
	`)
	if err != nil {
		return nil
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	captureNames := query.CaptureNames()
	var imports []ir.ImportIR

	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		var imp ir.ImportIR

		for _, capture := range match.Captures {
			name := captureNames[capture.Index]
			text := string(capture.Node.Utf8Text(src))

			switch name {
			case "path":
				// Strip surrounding quotes from the string literal.
				imp.Path = strings.Trim(text, `"`)
			case "alias":
				imp.Alias = text
			}
		}

		if imp.Path != "" {
			imports = append(imports, imp)
		}
	}

	return imports
}

// --- Function extraction ---

// extractFunctions extracts all function and method declarations.
//
// Two separate queries handle functions vs methods because their
// grammar structures differ significantly in Tree-sitter's Go grammar.
func (e *GoExtractor) extractFunctions(root *tree_sitter.Node, src []byte, pkg string) []ir.FunctionIR {
	var functions []ir.FunctionIR

	functions = append(functions, e.extractPlainFunctions(root, src, pkg)...)
	functions = append(functions, e.extractMethods(root, src, pkg)...)

	return functions
}

// extractPlainFunctions extracts top-level functions (no receiver).
//
//	func Foo() {}
//	func bar() {}
func (e *GoExtractor) extractPlainFunctions(root *tree_sitter.Node, src []byte, pkg string) []ir.FunctionIR {
	query, err := tree_sitter.NewQuery(e.tsLang, `
		(function_declaration
			name: (identifier) @name)
	`)
	if err != nil {
		return nil
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	var functions []ir.FunctionIR

	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		for _, capture := range match.Captures {
			name := string(capture.Node.Utf8Text(src))
			functions = append(functions, ir.FunctionIR{
				Name:       name,
				Qualified:  qualifiedName(pkg, "", name),
				Receiver:   "",
				Visibility: visibility(name),
			})
		}
	}

	return functions
}

// extractMethods extracts method declarations (with receiver).
//
//	func (s *Scanner) Scan() {}
//	func (p Parser) Parse() {}
//
// The receiver type is normalised — pointer indicators (*) are stripped
// so "(*Scanner)" and "(Scanner)" both produce Receiver: "Scanner".
func (e *GoExtractor) extractMethods(root *tree_sitter.Node, src []byte, pkg string) []ir.FunctionIR {
	// receiver_type captures the type inside the parameter declaration
	// within the method receiver. We capture it separately from the
	// function name to handle both pointer and value receivers cleanly.
	query, err := tree_sitter.NewQuery(e.tsLang, `
		(method_declaration
			receiver: (parameter_list
				(parameter_declaration
					type: [
						(type_identifier) @receiver_type
						(pointer_type (type_identifier) @receiver_type)
					]))
			name: (field_identifier) @name)
	`)
	if err != nil {
		return nil
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	captureNames := query.CaptureNames()
	var methods []ir.FunctionIR

	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		var name, receiver string

		for _, capture := range match.Captures {
			captureName := captureNames[capture.Index]
			text := string(capture.Node.Utf8Text(src))

			switch captureName {
			case "name":
				name = text
			case "receiver_type":
				receiver = text
			}
		}

		if name != "" {
			methods = append(methods, ir.FunctionIR{
				Name:       name,
				Qualified:  qualifiedName(pkg, receiver, name),
				Receiver:   receiver,
				Visibility: visibility(name),
			})
		}
	}

	return methods
}

// --- Symbol extraction ---

// extractSymbols extracts named type-level declarations.
//
// Covers:
//   - struct types
//   - interface types
//   - type aliases
//   - constants
//   - variables
func (e *GoExtractor) extractSymbols(root *tree_sitter.Node, src []byte, pkg string) []ir.SymbolIR {
	var symbols []ir.SymbolIR

	symbols = append(symbols, e.extractTypeDeclarations(root, src, pkg)...)
	symbols = append(symbols, e.extractConstVarDeclarations(root, src, pkg)...)

	return symbols
}

// extractTypeDeclarations extracts struct, interface, and alias declarations.
//
//	type Scanner struct {}      → kind: "struct"
//	type Reader interface {}    → kind: "interface"
//	type MyInt = int            → kind: "alias"
//	type MyInt int              → kind: "alias" (defined type, treated same)
func (e *GoExtractor) extractTypeDeclarations(root *tree_sitter.Node, src []byte, pkg string) []ir.SymbolIR {
	query, err := tree_sitter.NewQuery(e.tsLang, `
		(type_declaration
			(type_spec
				name: (type_identifier) @name
				type: [
					(struct_type)      @struct
					(interface_type)   @interface
					(type_identifier)  @alias
					(qualified_type)   @alias
				]))
	`)
	if err != nil {
		return nil
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	captureNames := query.CaptureNames()
	var symbols []ir.SymbolIR

	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		var name, kind string

		for _, capture := range match.Captures {
			captureName := captureNames[capture.Index]
			text := string(capture.Node.Utf8Text(src))

			switch captureName {
			case "name":
				name = text
			case "struct":
				kind = "struct"
			case "interface":
				kind = "interface"
			case "alias":
				if kind == "" {
					kind = "alias"
				}
			}
		}

		if name != "" && kind != "" {
			symbols = append(symbols, ir.SymbolIR{
				Name:       name,
				Qualified:  pkg + "." + name,
				Kind:       kind,
				Visibility: visibility(name),
			})
		}
	}

	return symbols
}

// extractConstVarDeclarations extracts top-level const and var declarations.
func (e *GoExtractor) extractConstVarDeclarations(root *tree_sitter.Node, src []byte, pkg string) []ir.SymbolIR {
	query, err := tree_sitter.NewQuery(e.tsLang, `
		[
			(const_declaration (const_spec name: (identifier) @name) @const)
			(var_declaration   (var_spec   name: (identifier) @name) @var)
		]
	`)
	if err != nil {
		return nil
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	captureNames := query.CaptureNames()
	var symbols []ir.SymbolIR

	// Track seen names to avoid duplicates from multi-value specs:
	// const ( A, B = 1, 2 ) produces two identifier captures.
	seen := make(map[string]bool)

	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		var name, kind string

		for _, capture := range match.Captures {
			captureName := captureNames[capture.Index]
			text := string(capture.Node.Utf8Text(src))

			switch captureName {
			case "name":
				name = text
			case "const":
				kind = "const"
			case "var":
				kind = "var"
			}
		}

		if name != "" && kind != "" && !seen[name] {
			seen[name] = true
			symbols = append(symbols, ir.SymbolIR{
				Name:       name,
				Qualified:  pkg + "." + name,
				Kind:       kind,
				Visibility: visibility(name),
			})
		}
	}

	return symbols
}

// --- Call extraction ---

// extractCalls extracts outbound call expressions from function bodies.
//
// Examples extracted:
//
//	fmt.Println("hello")   → Target: "fmt.Println",  Kind: external
//	s.Scan()               → Target: "s.Scan",        Kind: method
//	len(x)                 → Target: "len",            Kind: builtin
//	doSomething()          → Target: "doSomething",   Kind: internal
//
// Note: calls are extracted globally from the file, not per-function.
// The Caller field is set to the file's package for now. Per-function
// attribution will be added when the graph traversal layer needs it.
func (e *GoExtractor) extractCalls(root *tree_sitter.Node, src []byte, pkg string, fns []ir.FunctionIR) []ir.CallIR {
	query, err := tree_sitter.NewQuery(e.tsLang, `
		(call_expression
			function: [
				(identifier) @call
				(selector_expression
					operand: (identifier)
					field:   (field_identifier) @call_field)
			])
	`)
	if err != nil {
		return nil
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	captureNames := query.CaptureNames()

	// Deduplicate calls — the same call site may match multiple patterns.
	type callKey struct{ target, kind string }
	seen := make(map[callKey]bool)
	var calls []ir.CallIR

	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		for _, capture := range match.Captures {
			captureName := captureNames[capture.Index]
			text := string(capture.Node.Utf8Text(src))

			var target string
			var kind ir.CallKind

			switch captureName {
			case "call":
				// Simple identifier call: doSomething() or len()
				target = text
				if goBuiltins[target] {
					kind = ir.CallKindBuiltin
				} else {
					kind = ir.CallKindInternal
				}

			case "call_field":
				// Selector call: pkg.Func() or receiver.Method()
				// Build full target from the selector expression parent.
				parent := capture.Node.Parent()
				if parent != nil {
					target = string(parent.Utf8Text(src))
				} else {
					target = text
				}
				kind = ir.CallKindExternal
			}

			if target == "" {
				continue
			}

			key := callKey{target, string(kind)}
			if seen[key] {
				continue
			}
			seen[key] = true

			calls = append(calls, ir.CallIR{
				Caller: pkg,
				Target: target,
				Kind:   kind,
			})
		}
	}

	return calls
}

// --- Edge building ---

// buildEdges constructs EdgeIRs from the already-extracted IR fields.
//
// Edges are derived from imports, function definitions, and calls.
// This runs after all other extraction is complete.
func (e *GoExtractor) buildEdges(file *ir.FileIR) []ir.EdgeIR {
	var edges []ir.EdgeIR

	// Import edges: file → imported package
	for _, imp := range file.Imports {
		// Use the last segment of the import path as the package identifier.
		// "github.com/yourorg/mearch/internal/parser" → "parser"
		pkgName := importPackageName(imp)

		edges = append(edges, ir.EdgeIR{
			From: filepath.Base(file.Path),
			To:   pkgName,
			Kind: ir.EdgeKindImport,
		})
	}

	// Define edges: type → its methods
	// Build a quick lookup of receiver → methods.
	for _, fn := range file.Functions {
		if fn.Receiver != "" {
			edges = append(edges, ir.EdgeIR{
				From: file.Package + "." + fn.Receiver,
				To:   fn.Qualified,
				Kind: ir.EdgeKindDefine,
			})
		}
	}

	// Call edges: package → call target
	for _, call := range file.Calls {
		if call.Kind == ir.CallKindBuiltin {
			// Don't pollute the graph with builtin edges.
			continue
		}
		edges = append(edges, ir.EdgeIR{
			From: call.Caller,
			To:   call.Target,
			Kind: ir.EdgeKindCall,
		})
	}

	return edges
}
