package extractor

import (
	"path/filepath"
	"strings"

	"github.com/mohamamd-y-abbass/mearch/internal/ir"
	"github.com/mohamamd-y-abbass/mearch/internal/parser"
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_java "github.com/tree-sitter/tree-sitter-java/bindings/go"
)

// JavaExtractor extracts FileIR from Java source files.
// Handles: classes, interfaces, enums, methods, imports, calls.
type JavaExtractor struct {
	tsLang *tree_sitter.Language
}

// NewJavaExtractor constructs a JavaExtractor.
func NewJavaExtractor() *JavaExtractor {
	return &JavaExtractor{
		tsLang: tree_sitter.NewLanguage(tree_sitter_java.Language()),
	}
}

// Extract produces a FileIR from a parsed Java file.
func (e *JavaExtractor) Extract(result *parser.ParseResult) (*ir.FileIR, error) {
	file := &ir.FileIR{
		Path:     result.Path,
		Language: "java",
	}

	root := result.RootNode()
	src := result.Source
	lang := e.tsLang

	file.Package = e.extractPackage(lang, root, src, result.Path)
	file.Imports = e.extractImports(lang, root, src)
	file.Symbols = e.extractSymbols(lang, root, src, file.Package)
	file.Functions = e.extractMethods(lang, root, src, file.Package)
	file.Calls = e.extractCalls(lang, root, src, file.Package)
	file.Edges = e.buildEdges(file)

	return file, nil
}

func (e *JavaExtractor) extractPackage(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte, path string) string {
	query, err := tree_sitter.NewQuery(lang, `(package_declaration (scoped_identifier) @pkg)`)
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

	// Fallback: filename as package
	return strings.TrimSuffix(filepath.Base(path), ".java")
}

func (e *JavaExtractor) extractImports(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte) []ir.ImportIR {
	query, err := tree_sitter.NewQuery(lang, `(import_declaration (scoped_identifier) @path)`)
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

func (e *JavaExtractor) extractSymbols(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte, pkg string) []ir.SymbolIR {
	query, err := tree_sitter.NewQuery(lang, `
		[
			(class_declaration       name: (identifier) @name) @class
			(interface_declaration   name: (identifier) @name) @interface
			(enum_declaration        name: (identifier) @name) @enum
			(annotation_type_declaration name: (identifier) @name) @annotation
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
			case "class":
				kind = "struct"
			case "interface":
				kind = "interface"
			case "enum":
				kind = "const"
			case "annotation":
				kind = "alias"
			}
		}
		if name != "" && kind != "" && !seen[name] {
			seen[name] = true
			syms = append(syms, ir.SymbolIR{
				Name:       name,
				Qualified:  pkg + "." + name,
				Kind:       kind,
				Visibility: javaVisibility(name),
			})
		}
	}
	return syms
}

func (e *JavaExtractor) extractMethods(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte, pkg string) []ir.FunctionIR {
	query, err := tree_sitter.NewQuery(lang, `
		(method_declaration name: (identifier) @name)
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
				Qualified:  pkg + "." + name,
				Visibility: javaVisibility(name),
			})
		}
	}
	return fns
}

func (e *JavaExtractor) extractCalls(lang *tree_sitter.Language, root *tree_sitter.Node, src []byte, pkg string) []ir.CallIR {
	query, err := tree_sitter.NewQuery(lang, `
		(method_invocation name: (identifier) @call)
	`)
	if err != nil {
		return nil
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	type callKey struct{ target string }
	seen := make(map[callKey]bool)
	var calls []ir.CallIR

	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		for _, capture := range match.Captures {
			target := string(capture.Node.Utf8Text(src))
			if target == "" {
				continue
			}
			key := callKey{target}
			if !seen[key] {
				seen[key] = true
				calls = append(calls, ir.CallIR{
					Caller: pkg,
					Target: target,
					Kind:   ir.CallKindInternal,
				})
			}
		}
	}
	return calls
}

func (e *JavaExtractor) buildEdges(file *ir.FileIR) []ir.EdgeIR {
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

// javaVisibility uses Java convention: uppercase first letter = public by convention
// Real visibility requires detecting 'public'/'private' modifiers on the parent node.
func javaVisibility(name string) string {
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
