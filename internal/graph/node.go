package graph

import "fmt"

// NodeKind classifies what a node represents in the codebase.
// Used by the retrieval engine to filter and rank results.
type NodeKind string

const (
	NodeKindFile      NodeKind = "file"
	NodeKindPackage   NodeKind = "package"
	NodeKindFunction  NodeKind = "function"
	NodeKindMethod    NodeKind = "method"
	NodeKindStruct    NodeKind = "struct"
	NodeKindInterface NodeKind = "interface"
	NodeKindConst     NodeKind = "const"
	NodeKindVar       NodeKind = "var"
	NodeKindAlias     NodeKind = "alias"
	NodeKindExternal  NodeKind = "external" // third-party / stdlib package
)

// Visibility of a symbol.
type Visibility string

const (
	VisibilityExported   Visibility = "exported"
	VisibilityUnexported Visibility = "unexported"
)

// Node is a vertex in the code graph.
//
// Each node has a stable unique ID (its qualified name) and carries
// enough metadata for the retrieval engine to rank and describe it
// without needing to re-read the source file.
type Node struct {
	// ID is the unique, stable identifier for this node.
	// Format mirrors the IR qualified name convention:
	//   file      → absolute path: "/project/main.go"
	//   package   → package name: "scanner"
	//   function  → "package.FuncName"
	//   method    → "package.ReceiverType.MethodName"
	//   symbol    → "package.SymbolName"
	//   external  → import path: "github.com/some/pkg"
	ID string

	// Kind classifies what this node represents.
	Kind NodeKind

	// Name is the short display name (not qualified).
	// Used in retrieval output and logging.
	Name string

	// FilePath is the source file this node was extracted from.
	// Empty for package and external nodes.
	FilePath string

	// Package is the Go package this node belongs to.
	Package string

	// Visibility is exported/unexported.
	// Only meaningful for function, method, struct, interface, const, var nodes.
	Visibility Visibility
}

// String implements fmt.Stringer for debug output.
func (n *Node) String() string {
	return fmt.Sprintf("Node{id=%q kind=%s}", n.ID, n.Kind)
}
