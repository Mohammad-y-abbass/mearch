// Package graph implements the in-memory code graph for Mearch.
//
// The graph is the central data structure of the entire engine.
// Everything above this layer (retrieval, ranking, MCP) operates on
// the graph — never on raw IR or ASTs.
//
// Architecture position:
//
//	IR Extractor → [Graph] → Retrieval Engine
//
// Design decisions:
//
//  1. Directed graph — edges have direction (A imports B ≠ B imports A).
//  2. Typed nodes and edges — NodeKind and EdgeKind carry semantic meaning,
//     not just topology. The retrieval engine uses these to rank results.
//  3. Dual adjacency index — outgoing AND incoming edges are indexed so
//     traversal works in both directions:
//     "what does this file import?" and "who imports this package?" are
//     both O(1) lookups.
//  4. In-memory only for now — SQLite persistence comes in Phase 3.
//     The Graph struct is designed so persistence can be added without
//     changing the public API.
//  5. Concurrency-safe reads — sync.RWMutex allows many concurrent readers
//     (retrieval engine) with exclusive writes (builder/watcher).
package graph

import (
	"sync"
)

type Graph struct {
	mu sync.RWMutex

	// nodes is the primary node store: ID → Node.
	nodes map[string]*Node

	// out is the outgoing adjacency list: NodeID → []Edge leaving that node.
	// Used for forward traversal: "what does A depend on?"
	out map[string][]Edge

	// in is the incoming adjacency list: NodeID → []Edge arriving at that node.
	// Used for reverse traversal: "who depends on A?"
	in map[string][]Edge
}

// NewGraph constructs an empty Graph.
func NewGraph() *Graph {
	return &Graph{
		nodes: make(map[string]*Node),
		out:   make(map[string][]Edge),
		in:    make(map[string][]Edge),
	}
}

// AddNode adds a node to the graph.
//
// If a node with the same ID already exists, it is overwritten.
// This is intentional — when the same symbol is declared across multiple
// files (e.g. package-level vars split across files), the last write wins.
// The graph cares about relationships, not duplicate declarations.
func (g *Graph) AddNode(n *Node) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes[n.ID] = n
}

// AddEdge adds a directed edge to the graph.
//
// If either the From or To node does not exist yet, a placeholder node
// of kind NodeKindExternal is created automatically. This handles the
// common case of importing a stdlib or third-party package that has no
// source file in the scanned repository.
//
// Duplicate edges (same From, To, Kind) are silently ignored.
func (g *Graph) AddEdge(e Edge) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Auto-create placeholder nodes for unknown endpoints.
	if _, ok := g.nodes[e.From]; !ok {
		g.nodes[e.From] = &Node{
			ID:   e.From,
			Kind: NodeKindExternal,
			Name: e.From,
		}
	}
	if _, ok := g.nodes[e.To]; !ok {
		g.nodes[e.To] = &Node{
			ID:   e.To,
			Kind: NodeKindExternal,
			Name: e.To,
		}
	}

	// Deduplicate: skip if this exact edge already exists.
	for _, existing := range g.out[e.From] {
		if existing.To == e.To && existing.Kind == e.Kind {
			return
		}
	}

	// Index in both directions.
	g.out[e.From] = append(g.out[e.From], e)
	g.in[e.To] = append(g.in[e.To], e)
}

// Node returns the node with the given ID, or nil if not found.
func (g *Graph) Node(id string) *Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.nodes[id]
}

// Neighbors returns all nodes reachable from id via outgoing edges.
// Optionally filtered by edge kind — pass zero kinds to get all neighbors.
//
// Used by the retrieval engine for forward traversal:
// "given this file, what does it depend on?"
func (g *Graph) Neighbors(id string, kinds ...EdgeKind) []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()

	edges := g.out[id]
	result := make([]*Node, 0, len(edges))

	for _, e := range edges {
		if len(kinds) > 0 && !edgeKindMatch(e.Kind, kinds) {
			continue
		}
		if n, ok := g.nodes[e.To]; ok {
			result = append(result, n)
		}
	}

	return result
}

// Dependents returns all nodes that have an incoming edge to id.
// Optionally filtered by edge kind.
//
// Used by the retrieval engine for reverse traversal:
// "who calls this function?" or "which files import this package?"
func (g *Graph) Dependents(id string, kinds ...EdgeKind) []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()

	edges := g.in[id]
	result := make([]*Node, 0, len(edges))

	for _, e := range edges {
		if len(kinds) > 0 && !edgeKindMatch(e.Kind, kinds) {
			continue
		}
		if n, ok := g.nodes[e.From]; ok {
			result = append(result, n)
		}
	}

	return result
}

// OutEdges returns all outgoing edges from id.
func (g *Graph) OutEdges(id string) []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := g.out[id]
	result := make([]Edge, len(out))
	copy(result, out)
	return result
}

// InEdges returns all incoming edges to id.
func (g *Graph) InEdges(id string) []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	in := g.in[id]
	result := make([]Edge, len(in))
	copy(result, in)
	return result
}

// AllNodes returns all nodes in the graph.
// Order is not guaranteed (map iteration).
func (g *Graph) AllNodes() []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	nodes := make([]*Node, 0, len(g.nodes))
	for _, n := range g.nodes {
		nodes = append(nodes, n)
	}
	return nodes
}

// Stats returns a snapshot of graph size metrics.
// Useful for logging and debugging.
func (g *Graph) Stats() Stats {
	g.mu.RLock()
	defer g.mu.RUnlock()

	totalEdges := 0
	for _, edges := range g.out {
		totalEdges += len(edges)
	}

	// Count nodes by kind.
	byKind := make(map[NodeKind]int)
	for _, n := range g.nodes {
		byKind[n.Kind]++
	}

	return Stats{
		TotalNodes: len(g.nodes),
		TotalEdges: totalEdges,
		ByKind:     byKind,
	}
}



// BFS performs a breadth-first traversal starting from startID.
//
// It visits each reachable node once and calls visit(node, depth).
// If visit returns false, traversal stops immediately.
// maxDepth of 0 means unlimited depth.
//
// Used by the retrieval engine for localized context gathering:
// "find everything within N hops of this symbol".
func (g *Graph) BFS(startID string, maxDepth int, visit func(n *Node, depth int) bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	start, ok := g.nodes[startID]
	if !ok {
		return
	}

	type entry struct {
		node  *Node
		depth int
	}

	visited := make(map[string]bool)
	queue := []entry{{start, 0}}
	visited[startID] = true

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		if !visit(curr.node, curr.depth) {
			return
		}

		if maxDepth > 0 && curr.depth >= maxDepth {
			continue
		}

		for _, e := range g.out[curr.node.ID] {
			if !visited[e.To] {
				visited[e.To] = true
				if neighbor, ok := g.nodes[e.To]; ok {
					queue = append(queue, entry{neighbor, curr.depth + 1})
				}
			}
		}
	}
}

// DFS performs a depth-first traversal starting from startID.
//
// It visits each reachable node once and calls visit(node, depth).
// If visit returns false, that branch is pruned (but traversal continues
// on other branches).
// maxDepth of 0 means unlimited depth.
//
// Used by the retrieval engine for deep call chain exploration.
func (g *Graph) DFS(startID string, maxDepth int, visit func(n *Node, depth int) bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	visited := make(map[string]bool)
	g.dfs(startID, 0, maxDepth, visited, visit)
}

// dfs is the internal recursive DFS helper.
// Called with RLock already held — do NOT lock again inside.
func (g *Graph) dfs(id string, depth, maxDepth int, visited map[string]bool, visit func(*Node, int) bool) {
	if visited[id] {
		return
	}
	visited[id] = true

	node, ok := g.nodes[id]
	if !ok {
		return
	}

	if !visit(node, depth) {
		return
	}

	if maxDepth > 0 && depth >= maxDepth {
		return
	}

	for _, e := range g.out[id] {
		g.dfs(e.To, depth+1, maxDepth, visited, visit)
	}
}
