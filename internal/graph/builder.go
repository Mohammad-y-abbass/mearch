// Package graph (builder.go) implements the Builder that converts
// a collection of FileIRs into a populated Graph.
//
// The Builder is the only component that writes to the Graph during
// initial indexing. After Build() returns, the Graph is read-only
// until the watcher triggers an incremental update.
//
// Architecture position:
//
//	[]FileIR → [Builder] → *Graph → Retrieval Engine
package graph

import (
	"path/filepath"
	"strings"

	"github.com/mohamamd-y-abbass/mearch/internal/ir"
)

// Builder converts a collection of FileIRs into a Graph.
//
// Builder is stateless — all state lives in the Graph it produces.
// Safe to reuse across multiple Build() calls (each call produces
// an independent Graph).
type Builder struct{}

// NewBuilder constructs a Builder.
func NewBuilder() *Builder {
	return &Builder{}
}

// Build consumes a slice of FileIRs and returns a fully populated Graph.
//
// Build makes three passes over the IR:
//
//  1. Node pass — add all nodes (files, packages, functions, symbols).
//     This ensures every node exists before edges reference it.
//
//  2. Edge pass — add all edges derived from imports, defines, and calls.
//
//  3. External pass — any edge target that didn't get a proper node in
//     pass 1 (stdlib, third-party packages) is already handled by
//     Graph.AddEdge's auto-placeholder logic, so this is implicit.
//
// Build does not validate IRs. Malformed qualified names produce
// low-quality graph edges but do not cause panics.
func (b *Builder) Build(files []*ir.FileIR) *Graph {
	g := NewGraph()

	// --- Pass 1: Nodes ---
	for _, file := range files {
		b.addFileNodes(g, file)
	}

	// --- Pass 2: Edges ---
	for _, file := range files {
		b.addFileEdges(g, file)
	}

	return g
}

// BuildOne adds a single FileIR into an existing Graph.
//
// Used by the incremental indexer when a single file changes:
// reparse the file → extract IR → BuildOne to patch the graph.
//
// Note: BuildOne does NOT remove stale nodes/edges from the previous
// version of the file. Full stale-node cleanup requires tracking which
// nodes came from which file — that will be added with the watcher layer.
// For now, BuildOne is additive only.
func (b *Builder) BuildOne(g *Graph, file *ir.FileIR) {
	b.addFileNodes(g, file)
	b.addFileEdges(g, file)
}

// --- Node construction ---

// addFileNodes adds all nodes that originate from a single FileIR.
func (b *Builder) addFileNodes(g *Graph, file *ir.FileIR) {
	// File node — represents the source file itself.
	g.AddNode(&Node{
		ID:       file.Path,
		Kind:     NodeKindFile,
		Name:     filepath.Base(file.Path),
		FilePath: file.Path,
		Package:  file.Package,
	})

	// Package node — one per unique package name.
	// Multiple files share the same package node (same package name).
	// AddNode overwrites on collision which is fine — all files in a
	// package produce an identical package node.
	if file.Package != "" {
		g.AddNode(&Node{
			ID:      file.Package,
			Kind:    NodeKindPackage,
			Name:    file.Package,
			Package: file.Package,
		})
	}

	// Function and method nodes.
	for _, fn := range file.Functions {
		kind := NodeKindFunction
		if fn.Receiver != "" {
			kind = NodeKindMethod
		}
		g.AddNode(&Node{
			ID:         fn.Qualified,
			Kind:       kind,
			Name:       fn.Name,
			FilePath:   file.Path,
			Package:    file.Package,
			Visibility: visibilityFromString(fn.Visibility),
		})
	}

	// Symbol nodes (structs, interfaces, aliases, consts, vars).
	for _, sym := range file.Symbols {
		g.AddNode(&Node{
			ID:         sym.Qualified,
			Kind:       nodeKindFromSymbol(sym.Kind),
			Name:       sym.Name,
			FilePath:   file.Path,
			Package:    file.Package,
			Visibility: visibilityFromString(sym.Visibility),
		})
	}

	// External package nodes — one per import path.
	// These represent stdlib and third-party packages.
	// We add them in the node pass so edge deduplication works correctly.
	for _, imp := range file.Imports {
		if imp.Path == "" {
			continue
		}
		// Only add if not already present (another file may have imported
		// the same package).
		if g.Node(imp.Path) == nil {
			g.AddNode(&Node{
				ID:   imp.Path,
				Kind: NodeKindExternal,
				Name: importBaseName(imp),
			})
		}
	}
}

// --- Edge construction ---

// addFileEdges adds all edges that originate from a single FileIR.
func (b *Builder) addFileEdges(g *Graph, file *ir.FileIR) {
	b.addImportEdges(g, file)
	b.addDefineEdges(g, file)
	b.addCallEdges(g, file)
}

// addImportEdges adds file → package import edges.
//
//	main.go  -[import]→  fmt
//	main.go  -[import]→  github.com/yourorg/mearch/internal/scanner
func (b *Builder) addImportEdges(g *Graph, file *ir.FileIR) {
	for _, imp := range file.Imports {
		if imp.Path == "" {
			continue
		}
		g.AddEdge(Edge{
			From: file.Path,
			To:   imp.Path,
			Kind: EdgeKindImport,
		})

		// Also link package → imported package for package-level traversal.
		// This lets the retrieval engine answer: "what does the scanner
		// package depend on?" without needing to enumerate all its files.
		if file.Package != "" {
			g.AddEdge(Edge{
				From: file.Package,
				To:   imp.Path,
				Kind: EdgeKindImport,
			})
		}
	}
}

// addDefineEdges adds type → method define edges.
//
//	scanner.Scanner  -[define]→  scanner.Scanner.Scan
func (b *Builder) addDefineEdges(g *Graph, file *ir.FileIR) {
	for _, fn := range file.Functions {
		if fn.Receiver == "" {
			// Plain function — link package → function.
			if file.Package != "" {
				g.AddEdge(Edge{
					From: file.Package,
					To:   fn.Qualified,
					Kind: EdgeKindDefine,
				})
			}
		} else {
			// Method — link receiver type → method.
			receiverID := file.Package + "." + fn.Receiver
			g.AddEdge(Edge{
				From: receiverID,
				To:   fn.Qualified,
				Kind: EdgeKindDefine,
			})
		}
	}

	// Symbol definitions: package → symbol.
	for _, sym := range file.Symbols {
		if file.Package != "" {
			g.AddEdge(Edge{
				From: file.Package,
				To:   sym.Qualified,
				Kind: EdgeKindDefine,
			})
		}
	}
}

// addCallEdges adds caller → callee call edges.
//
//	main.main  -[call]→  fmt.Println
//	main.main  -[call]→  scanner.NewScanner
func (b *Builder) addCallEdges(g *Graph, file *ir.FileIR) {
	for _, call := range file.Calls {
		// Skip builtins — they pollute the graph without adding signal.
		if call.Kind == ir.CallKindBuiltin {
			continue
		}

		// Resolve the caller to a qualified name.
		// Currently Caller is set to the package name by the extractor.
		// When per-function attribution lands, this becomes fn.Qualified.
		from := call.Caller
		if from == "" {
			from = file.Package
		}

		g.AddEdge(Edge{
			From: from,
			To:   call.Target,
			Kind: EdgeKindCall,
		})
	}
}

// --- Helpers ---

// nodeKindFromSymbol maps an ir.SymbolIR.Kind string to a NodeKind.
func nodeKindFromSymbol(kind string) NodeKind {
	switch kind {
	case "struct":
		return NodeKindStruct
	case "interface":
		return NodeKindInterface
	case "const":
		return NodeKindConst
	case "var":
		return NodeKindVar
	case "alias":
		return NodeKindAlias
	default:
		return NodeKindAlias
	}
}

// visibilityFromString converts an ir visibility string to Visibility.
func visibilityFromString(v string) Visibility {
	if v == "exported" {
		return VisibilityExported
	}
	return VisibilityUnexported
}

// importBaseName returns the effective local name of an import for display.
func importBaseName(imp ir.ImportIR) string {
	if imp.Alias != "" && imp.Alias != "_" && imp.Alias != "." {
		return imp.Alias
	}
	parts := strings.Split(imp.Path, "/")
	return parts[len(parts)-1]
}
