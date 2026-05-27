package extractor

import (
	"path/filepath"
	"strings"

	"github.com/mohamamd-y-abbass/mearch/internal/ir"
	"github.com/mohamamd-y-abbass/mearch/internal/parser"
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
)

// RustExtractor extracts FileIR from Rust source files.
// Handles: functions, impl methods, structs, enums, traits, use declarations, calls.
type RustExtractor struct {
	tsLang *tree_sitter.Language
}

// NewRustExtractor constructs a RustExtractor.
func NewRustExtractor() *RustExtractor {
	return &RustExtractor{
		tsLang: tree_sitter.NewLanguage(tree_sitter_rust.Language()),
	}
}

// Extract produces a FileIR from a parsed Rust file.
func (e *RustExtractor) Extract(result *parser.ParseResult) (*ir.FileIR, error) {
	file := &ir.FileIR{
		Path:     result.Path,
		Language: "rust",
		Package:  rustModuleName(result.Path),
	}

	root := result.RootNode()
	src := result.Source
	lang := e.tsLang

	file.Imports = e.extractImports(lang, root, src)
	file.Functions = e.extractFunctions(lang, root, src, file.Package)
	file.Symbols = e.extractSymbols(lang, root, src, file.Package)
	file.Calls = e.extractCalls(lang, root, src, file.Package)
	file.Edges = e.buildEdges(file)

	return file, nil
}

func (e *RustExtractor) extractImports(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte) []ir.ImportIR {
	// use std::io;
	// use std::io::Write;
	// use crate::scanner::Scanner;
	query, err := tree_sitter.NewQuery(lang, `
		(use_declaration argument: (_) @path)
	`)
	if err != nil {
		return nil
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	var imports []ir.ImportIR
	seen := make(map[string]bool)

	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		for _, capture := range match.Captures {
			path := string(capture.Node.Utf8Text(src))
			// Normalise :: to / for consistency
			path = strings.ReplaceAll(path, "::", "/")
			if path != "" && !seen[path] {
				seen[path] = true
				imports = append(imports, ir.ImportIR{Path: path})
			}
		}
	}
	return imports
}

func (e *RustExtractor) extractFunctions(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte, module string) []ir.FunctionIR {
	// Top-level functions and impl methods
	query, err := tree_sitter.NewQuery(lang, `
		[
			(function_item name: (identifier) @name)
			(function_signature_item name: (identifier) @name)
		]
	`)
	if err != nil {
		return nil
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	seen := make(map[string]bool)
	var fns []ir.FunctionIR

	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		for _, capture := range match.Captures {
			name := string(capture.Node.Utf8Text(src))
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			fns = append(fns, ir.FunctionIR{
				Name:       name,
				Qualified:  module + "::" + name,
				Visibility: rustVisibility(&capture.Node, src),
			})
		}
	}
	return fns
}

func (e *RustExtractor) extractSymbols(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte, module string) []ir.SymbolIR {
	query, err := tree_sitter.NewQuery(lang, `
		[
			(struct_item  name: (type_identifier) @name) @struct
			(enum_item    name: (type_identifier) @name) @enum
			(trait_item   name: (type_identifier) @name) @trait
			(type_item    name: (type_identifier) @name) @alias
		]
	`)
	if err != nil {
		return nil
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	captureNames := query.CaptureNames()
	seen := make(map[string]bool)
	var syms []ir.SymbolIR

	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		var name, kind string
		for _, capture := range match.Captures {
			text := string(capture.Node.Utf8Text(src))
			switch captureNames[capture.Index] {
			case "name":
				name = text
			case "struct":
				kind = "struct"
			case "enum":
				kind = "const" // enums map to const for graph consistency
			case "trait":
				kind = "interface"
			case "alias":
				kind = "alias"
			}
		}
		if name != "" && kind != "" && !seen[name] {
			seen[name] = true
			syms = append(syms, ir.SymbolIR{
				Name:       name,
				Qualified:  module + "::" + name,
				Kind:       kind,
				Visibility: "exported", // Rust: public by default if no pub keyword detection
			})
		}
	}
	return syms
}

func (e *RustExtractor) extractCalls(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte, module string) []ir.CallIR {
	query, err := tree_sitter.NewQuery(lang, `
		(call_expression function: [
			(identifier) @call
			(field_expression
				field: (field_identifier) @call_field)
			(scoped_identifier
				name: (identifier) @call_scoped)
		])
	`)
	if err != nil {
		return nil
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	captureNames := query.CaptureNames()

	type callKey struct{ target, kind string }
	seen := make(map[callKey]bool)
	var calls []ir.CallIR

	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		for _, capture := range match.Captures {
			text := string(capture.Node.Utf8Text(src))
			var target string
			var kind ir.CallKind

			switch captureNames[capture.Index] {
			case "call":
				target = text
				kind = ir.CallKindInternal
			case "call_field":
				parent := capture.Node.Parent()
				if parent != nil {
					target = string(parent.Utf8Text(src))
				} else {
					target = text
				}
				kind = ir.CallKindMethod
			case "call_scoped":
				parent := capture.Node.Parent()
				if parent != nil {
					target = strings.ReplaceAll(string(parent.Utf8Text(src)), "::", ".")
				} else {
					target = text
				}
				kind = ir.CallKindExternal
			}

			if target == "" {
				continue
			}
			key := callKey{target, string(kind)}
			if !seen[key] {
				seen[key] = true
				calls = append(calls, ir.CallIR{
					Caller: module,
					Target: target,
					Kind:   kind,
				})
			}
		}
	}
	return calls
}

func (e *RustExtractor) buildEdges(file *ir.FileIR) []ir.EdgeIR {
	var edges []ir.EdgeIR
	for _, imp := range file.Imports {
		edges = append(edges, ir.EdgeIR{
			From: filepath.Base(file.Path),
			To:   imp.Path,
			Kind: ir.EdgeKindImport,
		})
	}
	for _, call := range file.Calls {
		edges = append(edges, ir.EdgeIR{
			From: call.Caller,
			To:   call.Target,
			Kind: ir.EdgeKindCall,
		})
	}
	return edges
}

func rustModuleName(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// rustVisibility checks for pub keyword on the parent node.
func rustVisibility(node *tree_sitter.Node, src []byte) string {
	parent := node.Parent()
	if parent == nil {
		return "unexported"
	}
	text := string(parent.Utf8Text(src))
	if strings.HasPrefix(text, "pub") {
		return "exported"
	}
	return "unexported"
}
