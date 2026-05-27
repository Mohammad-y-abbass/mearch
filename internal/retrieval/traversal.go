package retrieval

import (
	"container/heap"

	"github.com/mohamamd-y-abbass/mearch/internal/graph"
)

// Traversal configuration.
const (
	maxTraversalDepth = 3    // hops from seed to explore
	maxCandidates     = 200  // candidate pool cap before scoring
	minEdgeWeight     = 0.15 // edges below this weight are not followed
)

// edgeWeights maps edge kinds to traversal weights.
// Higher = more likely to lead to relevant context.
//
// Rationale:
//
//	define:    tightest possible relationship (Scanner defines Scan)
//	call:      direct usage — A calls B means they're closely related
//	use/impl:  type-level relationships — strong semantic coupling
//	compose:   struct embedding — structural coupling
//	import:    looser — A imports B doesn't mean B is relevant
//	external:  almost never what you want
var edgeWeights = map[graph.EdgeKind]float64{
	graph.EdgeKindDefine:    0.90,
	graph.EdgeKindCall:      0.80,
	graph.EdgeKindUse:       0.70,
	graph.EdgeKindImplement: 0.70,
	graph.EdgeKindCompose:   0.65,
	graph.EdgeKindInherit:   0.65,
	graph.EdgeKindImport:    0.45,
}

// depthDecay maps traversal depth to a score multiplier.
// Nodes closer to seeds score higher than distant ones.
var depthDecay = map[int]float64{
	0: 1.00,
	1: 0.80,
	2: 0.55,
	3: 0.30,
}

// Candidate is a node collected during traversal, annotated with
// how it was reached. Input to the scoring phase.
type Candidate struct {
	Node       *graph.Node
	Depth      int
	Direction  string  // "seed", "forward", "backward"
	SeedID     string  // which seed led here
	EdgeWeight float64 // weight of the connecting edge
	PathScore  float64 // cumulative path score from seed
}

// Traverser performs bidirectional weighted BFS from seed nodes.
// Safe for concurrent use — all state is local to each Traverse call.
type Traverser struct {
	g *graph.Graph
}

// NewTraverser constructs a Traverser.
func NewTraverser(g *graph.Graph) *Traverser {
	return &Traverser{g: g}
}

// Traverse performs bidirectional weighted BFS from all seeds.
//
// For each seed:
//   - Forward BFS:  follows outgoing edges (what does this depend on?)
//   - Backward BFS: follows incoming edges (who depends on this?)
//
// IntentHints from seed detection influence traversal:
//   - PreferBackward: backward traversal gets depth +1 and score boost
//   - PreferForward:  forward traversal gets depth +1 and score boost
//   - IsCallerQuery:  only backward traversal is run
//
// Returns a deduplicated candidate pool. When a node is reachable
// from multiple seeds, the highest PathScore wins.
func (t *Traverser) Traverse(seeds []Seed, hints *IntentHints) []Candidate {
	best := make(map[string]*Candidate, maxCandidates)

	for _, seed := range seeds {
		seedNode := t.g.Node(seed.NodeID)
		if seedNode == nil {
			continue
		}

		// Seed itself is always a candidate at depth 0.
		updateBest(best, &Candidate{
			Node:       seedNode,
			Depth:      0,
			Direction:  "seed",
			SeedID:     seed.NodeID,
			EdgeWeight: 1.0,
			PathScore:  seed.Score,
		})

		// Determine traversal depths based on intent hints.
		forwardDepth := maxTraversalDepth
		backwardDepth := maxTraversalDepth

		if hints != nil {
			if hints.IsCallerQuery {
				// Pure caller query — only go backward.
				forwardDepth = 0
			}
			if hints.IsImportQuery {
				// Import query — only go forward.
				backwardDepth = 0
			}
			if hints.PreferBackward {
				// Boost backward depth by 1.
				backwardDepth = min3Int(backwardDepth+1, 4, 4)
			}
			if hints.PreferForward {
				forwardDepth = min3Int(forwardDepth+1, 4, 4)
			}
		}

		// Forward traversal — dependencies.
		if forwardDepth > 0 {
			t.bfs(seed.NodeID, seed.Score, "forward", false, forwardDepth, best)
		}

		// Backward traversal — callers / dependents.
		// Run backward first when PreferBackward is set.
		if backwardDepth > 0 {
			t.bfs(seed.NodeID, seed.Score, "backward", true, backwardDepth, best)
		}

		if len(best) >= maxCandidates {
			break
		}
	}

	// Convert to slice.
	candidates := make([]Candidate, 0, len(best))
	for _, c := range best {
		candidates = append(candidates, *c)
	}
	return candidates
}

// bfs performs weighted BFS in one direction from startID.
//
// Uses a max-heap priority queue ordered by PathScore so higher-weight
// paths fill the candidate budget before lower-weight ones.
// This ensures the maxCandidates budget is filled with the best candidates.
func (t *Traverser) bfs(
	startID string,
	seedScore float64,
	direction string,
	reverse bool,
	maxDepth int,
	best map[string]*Candidate,
) {
	pq := &pq{}
	heap.Init(pq)
	heap.Push(pq, &pqEntry{
		nodeID:    startID,
		depth:     0,
		pathScore: seedScore,
	})

	visited := make(map[string]bool)
	visited[startID] = true

	for pq.Len() > 0 {
		item := heap.Pop(pq).(*pqEntry)

		if item.depth >= maxDepth {
			continue
		}
		if len(best) >= maxCandidates {
			return
		}

		// Get edges in the appropriate direction.
		var edges []graph.Edge
		if reverse {
			edges = t.g.InEdges(item.nodeID)
		} else {
			edges = t.g.OutEdges(item.nodeID)
		}

		for _, edge := range edges {
			// Which end are we traversing to?
			nextID := edge.To
			if reverse {
				nextID = edge.From
			}

			if visited[nextID] {
				continue
			}

			// Look up edge weight. Unknown edge kinds get a low default.
			weight, ok := edgeWeights[edge.Kind]
			if !ok {
				weight = 0.25
			}
			if weight < minEdgeWeight {
				continue
			}

			nextNode := t.g.Node(nextID)
			if nextNode == nil {
				continue
			}

			// External nodes are high-centrality noise — skip them
			// beyond depth 1. At depth 1 they're useful context
			// (direct imports), deeper they're just stdlib noise.
			if nextNode.Kind == graph.NodeKindExternal && item.depth >= 1 {
				continue
			}

			// PathScore decays with depth and edge weight.
			decay := depthDecay[item.depth+1]
			if decay == 0 {
				decay = 0.15
			}
			pathScore := item.pathScore * weight * decay

			visited[nextID] = true

			updateBest(best, &Candidate{
				Node:       nextNode,
				Depth:      item.depth + 1,
				Direction:  direction,
				SeedID:     startID,
				EdgeWeight: weight,
				PathScore:  pathScore,
			})

			heap.Push(pq, &pqEntry{
				nodeID:    nextID,
				depth:     item.depth + 1,
				pathScore: pathScore,
			})
		}
	}
}

// updateBest updates the best candidate map.
// If the node is already present, replaces only if new PathScore is higher.
func updateBest(best map[string]*Candidate, c *Candidate) {
	if existing, ok := best[c.Node.ID]; ok {
		if c.PathScore > existing.PathScore {
			best[c.Node.ID] = c
		}
		return
	}
	best[c.Node.ID] = c
}

// --- Priority Queue ---

type pqEntry struct {
	nodeID    string
	depth     int
	pathScore float64
	index     int
}

type pq []*pqEntry

func (p pq) Len() int           { return len(p) }
func (p pq) Less(i, j int) bool { return p[i].pathScore > p[j].pathScore } // max-heap
func (p pq) Swap(i, j int)      { p[i], p[j] = p[j], p[i]; p[i].index = i; p[j].index = j }
func (p *pq) Push(x any)        { e := x.(*pqEntry); e.index = len(*p); *p = append(*p, e) }
func (p *pq) Pop() any {
	old := *p
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	*p = old[:n-1]
	return e
}
