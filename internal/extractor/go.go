package extractor

import (
	"path/filepath"
	"strings"
	"unicode"

	"github.com/mohamamd-y-abbass/mearch/internal/ir"
	"github.com/mohamamd-y-abbass/mearch/internal/parser"
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
)

// goBuiltins is the set of Go builtin function names.
// Used to classify CallIRs as CallKindBuiltin so they are not added
// to the graph as call edges — they would pollute every node's adjacency.
var goBuiltins = map[string]bool{
	"len": true, "cap": true, "make": true, "new": true,
	"append": true, "copy": true, "delete": true, "close": true,
	"panic": true, "recover": true, "print": true, "println": true,
	"real": true, "imag": true, "complex": true, "clear": true,
	"min": true, "max": true,
}

// GoExtractor extracts FileIR from parsed Go source files.
// Stateless — safe to reuse across files and goroutines.
type GoExtractor struct {
	tsLang *tree_sitter.Language
}

// NewGoExtractor constructs a GoExtractor.
func NewGoExtractor() *GoExtractor {
	return &GoExtractor{
		tsLang: tree_sitter.NewLanguage(tree_sitter_go.Language()),
	}
}

// Extract produces a FileIR from a parsed Go file.
func (e *GoExtractor) Extract(result *parser.ParseResult) (*ir.FileIR, error) {
	file := &ir.FileIR{
		Path:     result.Path,
		Language: "go",
	}

	root := result.RootNode()
	src := result.Source

	file.Package = e.extractPackage(root, src)
	file.Imports = e.extractImports(root, src)
	file.Functions = e.extractFunctions(root, src, file.Package)
	file.Symbols = e.extractSymbols(root, src, file.Package)
	file.Calls = e.extractCalls(root, src, file.Package)
	file.Edges = e.buildEdges(file)

	return file, nil
}

// --- Package ---

func (e *GoExtractor) extractPackage(root *tree_sitter.Node, src []byte) string {
	query, err := tree_sitter.NewQuery(e.tsLang, `(package_clause (package_identifier) @pkg)`)
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

// --- Imports ---

func (e *GoExtractor) extractImports(root *tree_sitter.Node, src []byte) []ir.ImportIR {
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
			text := string(capture.Node.Utf8Text(src))
			switch captureNames[capture.Index] {
			case "path":
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

// --- Functions ---

func (e *GoExtractor) extractFunctions(root *tree_sitter.Node, src []byte, pkg string) []ir.FunctionIR {
	var fns []ir.FunctionIR
	fns = append(fns, e.extractPlainFunctions(root, src, pkg)...)
	fns = append(fns, e.extractMethods(root, src, pkg)...)
	return fns
}

func (e *GoExtractor) extractPlainFunctions(root *tree_sitter.Node, src []byte, pkg string) []ir.FunctionIR {
	query, err := tree_sitter.NewQuery(e.tsLang, `(function_declaration name: (identifier) @name)`)
	if err != nil {
		return nil
	}
	defer query.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	var fns []ir.FunctionIR
	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		for _, capture := range match.Captures {
			name := string(capture.Node.Utf8Text(src))
			fns = append(fns, ir.FunctionIR{
				Name:       name,
				Qualified:  pkg + "." + name,
				Receiver:   "",
				Visibility: goVisibility(name),
			})
		}
	}
	return fns
}

func (e *GoExtractor) extractMethods(root *tree_sitter.Node, src []byte, pkg string) []ir.FunctionIR {
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
			text := string(capture.Node.Utf8Text(src))
			switch captureNames[capture.Index] {
			case "name":
				name = text
			case "receiver_type":
				receiver = text
			}
		}
		if name != "" {
			methods = append(methods, ir.FunctionIR{
				Name:       name,
				Qualified:  pkg + "." + receiver + "." + name,
				Receiver:   receiver,
				Visibility: goVisibility(name),
			})
		}
	}
	return methods
}

// --- Symbols ---

func (e *GoExtractor) extractSymbols(root *tree_sitter.Node, src []byte, pkg string) []ir.SymbolIR {
	var syms []ir.SymbolIR
	syms = append(syms, e.extractTypeDecls(root, src, pkg)...)
	syms = append(syms, e.extractConstVarDecls(root, src, pkg)...)
	return syms
}

func (e *GoExtractor) extractTypeDecls(root *tree_sitter.Node, src []byte, pkg string) []ir.SymbolIR {
	query, err := tree_sitter.NewQuery(e.tsLang, `
		(type_declaration
			(type_spec
				name: (type_identifier) @name
				type: [
					(struct_type)    @struct
					(interface_type) @interface
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
			case "interface":
				kind = "interface"
			case "alias":
				if kind == "" {
					kind = "alias"
				}
			}
		}
		if name != "" && kind != "" {
			syms = append(syms, ir.SymbolIR{
				Name:       name,
				Qualified:  pkg + "." + name,
				Kind:       kind,
				Visibility: goVisibility(name),
			})
		}
	}
	return syms
}

func (e *GoExtractor) extractConstVarDecls(root *tree_sitter.Node, src []byte, pkg string) []ir.SymbolIR {
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
	var syms []ir.SymbolIR
	seen := make(map[string]bool)

	matches := qc.Matches(query, root, src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		var name, kind string
		for _, capture := range match.Captures {
			text := string(capture.Node.Utf8Text(src))
			switch captureNames[capture.Index] {
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
			syms = append(syms, ir.SymbolIR{
				Name:       name,
				Qualified:  pkg + "." + name,
				Kind:       kind,
				Visibility: goVisibility(name),
			})
		}
	}
	return syms
}

// --- Calls ---

func (e *GoExtractor) extractCalls(root *tree_sitter.Node, src []byte, pkg string) []ir.CallIR {
	query, err := tree_sitter.NewQuery(e.tsLang, `
		(call_expression
			function: [
				(identifier) @call
				(selector_expression
					operand: (identifier)
					field: (field_identifier) @call_field)
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
				if goBuiltins[target] {
					kind = ir.CallKindBuiltin
				} else {
					kind = ir.CallKindInternal
				}
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
				Caller: pkg,
				Target: target,
				Kind:   kind,
			})
		}
	}
	return calls
}

// --- Edges ---

func (e *GoExtractor) buildEdges(file *ir.FileIR) []ir.EdgeIR {
	var edges []ir.EdgeIR

	for _, imp := range file.Imports {
		pkgName := goImportBaseName(imp)
		edges = append(edges, ir.EdgeIR{
			From: filepath.Base(file.Path),
			To:   pkgName,
			Kind: ir.EdgeKindImport,
		})
	}

	for _, fn := range file.Functions {
		if fn.Receiver != "" {
			edges = append(edges, ir.EdgeIR{
				From: file.Package + "." + fn.Receiver,
				To:   fn.Qualified,
				Kind: ir.EdgeKindDefine,
			})
		}
	}

	for _, call := range file.Calls {
		if call.Kind == ir.CallKindBuiltin {
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

// --- Helpers ---

func goVisibility(name string) string {
	if name == "" {
		return "unexported"
	}
	for _, r := range name {
		if unicode.IsUpper(r) {
			return "exported"
		}
	}
	return "unexported"
}

func goImportBaseName(imp ir.ImportIR) string {
	if imp.Alias != "" && imp.Alias != "_" && imp.Alias != "." {
		return imp.Alias
	}
	parts := strings.Split(imp.Path, "/")
	return parts[len(parts)-1]
}
