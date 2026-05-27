package extractor

import (
	"path/filepath"
	"strings"

	"github.com/mohamamd-y-abbass/mearch/internal/ir"
	"github.com/mohamamd-y-abbass/mearch/internal/parser"
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"
)

// PythonExtractor extracts FileIR from Python source files.
// Handles: functions, classes, methods, imports (import/from-import), calls.
type PythonExtractor struct {
	tsLang *tree_sitter.Language
}

// NewPythonExtractor constructs a PythonExtractor.
func NewPythonExtractor() *PythonExtractor {
	return &PythonExtractor{
		tsLang: tree_sitter.NewLanguage(tree_sitter_python.Language()),
	}
}

// Extract produces a FileIR from a parsed Python file.
func (e *PythonExtractor) Extract(result *parser.ParseResult) (*ir.FileIR, error) {
	file := &ir.FileIR{
		Path:     result.Path,
		Language: "python",
		Package:  pyModuleName(result.Path),
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

func (e *PythonExtractor) extractImports(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte) []ir.ImportIR {
	// Handles:
	//   import os
	//   import os.path
	//   from os import path
	//   from os.path import join, exists
	query, err := tree_sitter.NewQuery(lang, `
		[
			(import_statement
				name: (dotted_name) @path)
			(import_from_statement
				module_name: (dotted_name) @path)
			(import_from_statement
				module_name: (relative_import) @path)
		]
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
			if path != "" && !seen[path] {
				seen[path] = true
				imports = append(imports, ir.ImportIR{Path: path})
			}
		}
	}
	return imports
}

func (e *PythonExtractor) extractFunctions(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte, module string) []ir.FunctionIR {
	// function_definition covers both regular functions and methods
	query, err := tree_sitter.NewQuery(lang, `
		(function_definition name: (identifier) @name)
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
				Qualified:  module + "." + name,
				Visibility: pyVisibility(name),
			})
		}
	}
	return fns
}

func (e *PythonExtractor) extractSymbols(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte, module string) []ir.SymbolIR {
	query, err := tree_sitter.NewQuery(lang, `
		(class_definition name: (identifier) @name)
	`)
	if err != nil {
		return nil
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	seen := make(map[string]bool)
	var syms []ir.SymbolIR

	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		for _, capture := range match.Captures {
			name := string(capture.Node.Utf8Text(src))
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			syms = append(syms, ir.SymbolIR{
				Name:       name,
				Qualified:  module + "." + name,
				Kind:       "struct",
				Visibility: pyVisibility(name),
			})
		}
	}
	return syms
}

func (e *PythonExtractor) extractCalls(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte, module string) []ir.CallIR {
	query, err := tree_sitter.NewQuery(lang, `
		(call function: [
			(identifier) @call
			(attribute
				object: (identifier)
				attribute: (identifier) @call_field)
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

func (e *PythonExtractor) buildEdges(file *ir.FileIR) []ir.EdgeIR {
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

func pyModuleName(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// pyVisibility uses Python convention: _name = private, name = public
func pyVisibility(name string) string {
	if strings.HasPrefix(name, "_") {
		return "unexported"
	}
	return "exported"
}
