package extractor

import (
	"path/filepath"
	"strings"

	"github.com/mohamamd-y-abbass/mearch/internal/ir"
	"github.com/mohamamd-y-abbass/mearch/internal/parser"
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// TypeScriptExtractor extracts FileIR from TypeScript and TSX files.
// Handles: interfaces, type aliases, classes, functions, arrow functions,
// React components, hooks, imports/exports.
type TypeScriptExtractor struct {
	tsLang  *tree_sitter.Language // TypeScript grammar
	tsxLang *tree_sitter.Language // TSX grammar
}

// NewTypeScriptExtractor constructs a TypeScriptExtractor.
func NewTypeScriptExtractor() *TypeScriptExtractor {
	return &TypeScriptExtractor{
		tsLang:  tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTypescript()),
		tsxLang: tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTSX()),
	}
}

// Extract produces a FileIR from a parsed TypeScript or TSX file.
func (e *TypeScriptExtractor) Extract(result *parser.ParseResult) (*ir.FileIR, error) {
	file := &ir.FileIR{
		Path:     result.Path,
		Language: result.Language.String(),
	}

	// Choose grammar based on language
	lang := e.tsLang
	if result.Language == parser.LanguageTSX {
		lang = e.tsxLang
	}

	root := result.RootNode()
	src := result.Source

	// Module name derived from file path (no package declarations in TS)
	file.Package = tsModuleName(result.Path)

	file.Imports = e.extractImports(lang, root, src)
	file.Functions = e.extractFunctions(lang, root, src, file.Package)
	file.Symbols = e.extractSymbols(lang, root, src, file.Package)
	file.Calls = e.extractCalls(lang, root, src, file.Package)
	file.Edges = e.buildEdges(file)

	return file, nil
}

// --- Imports ---

func (e *TypeScriptExtractor) extractImports(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte) []ir.ImportIR {
	// Handles:
	//   import foo from 'bar'
	//   import { foo } from 'bar'
	//   import * as foo from 'bar'
	//   import type { Foo } from 'bar'
	query, err := tree_sitter.NewQuery(lang, `
		(import_statement
			source: (string) @path)
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
			raw := string(capture.Node.Utf8Text(src))
			path := strings.Trim(raw, `"'`+"`")
			if path != "" && !seen[path] {
				seen[path] = true
				imports = append(imports, ir.ImportIR{Path: path})
			}
		}
	}
	return imports
}

// --- Functions ---

func (e *TypeScriptExtractor) extractFunctions(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte, module string) []ir.FunctionIR {
	var fns []ir.FunctionIR

	// Regular function declarations: function foo() {}
	// Method definitions inside classes: methodName() {}
	// Arrow function variables: const foo = () => {}
	query, err := tree_sitter.NewQuery(lang, `
		[
			(function_declaration name: (identifier) @name)
			(function_signature   name: (identifier) @name)
			(method_definition    name: (property_identifier) @name)
			(lexical_declaration
				(variable_declarator
					name: (identifier) @name
					value: [(arrow_function) (function_expression)]))
		]
	`)
	if err != nil {
		return fns
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	seen := make(map[string]bool)
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
				Qualified:  module + "." + name,
				Receiver:   "",
				Visibility: tsVisibility(name),
			})
		}
	}

	return fns
}

// --- Symbols ---

func (e *TypeScriptExtractor) extractSymbols(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte, module string) []ir.SymbolIR {
	var syms []ir.SymbolIR

	// Interfaces, type aliases, classes, enums
	query, err := tree_sitter.NewQuery(lang, `
		[
			(interface_declaration  name: (type_identifier) @name) @interface
			(type_alias_declaration name: (type_identifier) @name) @alias
			(class_declaration      name: (type_identifier) @name) @class
			(enum_declaration       name: (identifier)      @name) @enum
		]
	`)
	if err != nil {
		return syms
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	captureNames := query.CaptureNames()
	seen := make(map[string]bool)

	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		var name, kind string
		for _, capture := range match.Captures {
			text := string(capture.Node.Utf8Text(src))
			switch captureNames[capture.Index] {
			case "name":
				name = text
			case "interface":
				kind = "interface"
			case "alias":
				kind = "alias"
			case "class":
				kind = "struct" // map class → struct for graph consistency
			case "enum":
				kind = "const"
			}
		}
		if name != "" && kind != "" && !seen[name] {
			seen[name] = true
			syms = append(syms, ir.SymbolIR{
				Name:       name,
				Qualified:  module + "." + name,
				Kind:       kind,
				Visibility: tsVisibility(name),
			})
		}
	}

	return syms
}

// --- Calls ---

func (e *TypeScriptExtractor) extractCalls(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte, module string) []ir.CallIR {
	query, err := tree_sitter.NewQuery(lang, `
		(call_expression
			function: [
				(identifier) @call
				(member_expression
					object: (identifier)
					property: (property_identifier) @call_field)
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
				Caller: module,
				Target: target,
				Kind:   kind,
			})
		}
	}
	return calls
}

// --- Edges ---

func (e *TypeScriptExtractor) buildEdges(file *ir.FileIR) []ir.EdgeIR {
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

// --- Helpers ---

// tsModuleName derives a module name from the file path.
// TypeScript has no package declarations — we use the filename without extension.
// e.g. "src/components/Button.tsx" → "Button"
func tsModuleName(path string) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext)
}

// tsVisibility checks if a TypeScript name is exported.
// In TypeScript, exported names are explicitly marked with 'export' keyword.
// Since we can't easily detect that from the name alone, we use PascalCase
// as a heuristic — React components and public types are PascalCase by convention.
func tsVisibility(name string) string {
	if name == "" {
		return "unexported"
	}
	for _, r := range name {
		if r >= 'A' && r <= 'Z' {
			return "exported"
		}
	}
	return "unexported"
}
