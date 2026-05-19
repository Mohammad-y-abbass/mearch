package graph

import "fmt"

// EdgeKind classifies the semantic relationship an edge represents.
// Mirrors ir.EdgeKind but lives in this package to keep graph self-contained.
type EdgeKind string

const (
	EdgeKindImport    EdgeKind = "import"
	EdgeKindCall      EdgeKind = "call"
	EdgeKindDefine    EdgeKind = "define"
	EdgeKindUse       EdgeKind = "use"
	EdgeKindInherit   EdgeKind = "inherit"
	EdgeKindImplement EdgeKind = "implement"
	EdgeKindCompose   EdgeKind = "compose"
)

// Edge is a directed relationship between two nodes.
//
// Edges are stored in both the outgoing index (From → []Edge) and the
// incoming index (To → []Edge) so traversal works in both directions.
type Edge struct {
	// From is the ID of the source node.
	From string

	// To is the ID of the target node.
	To string

	// Kind is the semantic relationship this edge represents.
	Kind EdgeKind
}

// String implements fmt.Stringer for debug output.
func (e *Edge) String() string {
	return fmt.Sprintf("Edge{%s -[%s]→ %s}", e.From, e.Kind, e.To)
}

// Graph is the in-memory code graph.
//
// Concurrency: safe for concurrent reads via RLock.
// Writes (AddNode, AddEdge) take an exclusive Lock.
// For bulk operations (building the graph from IR), prefer building
// without locks and then swapping the graph pointer atomically.
