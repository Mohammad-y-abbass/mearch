package retrieval

import (
	"math"

	"github.com/mohamamd-y-abbass/mearch/internal/graph"
	"github.com/mohamamd-y-abbass/mearch/internal/ir"
)

// GraphScores holds all pre-computed graph signal scores for a node.
// Computed once at index time, used at query time for fast scoring.
type GraphScores struct {
	// PageRank centrality score, normalized to [0,1].
	// External nodes discounted by 0.1x.
	PageRank float64

	// CallerCount is the number of nodes that call this node,
	// normalized by the maximum caller count in the graph.
	CallerCount float64

	// Betweenness is an approximation of betweenness centrality.
	// Full betweenness is O(n³) — we use (in_degree × out_degree) / total_edges.
	Betweenness float64
}

// GraphScorer pre-computes all graph-structural scores at index time.
// All scores are normalized to [0, 1] for consistent fusion weighting.
type GraphScorer struct {
	scores    map[string]*GraphScores    // nodeID → scores
	cooccur   map[string]map[string]bool // nodeID → set of co-occurring nodeIDs
	siblingOf map[string][]string        // nodeID → sibling nodeIDs
}

// NewGraphScorer builds all graph signal scores from the graph and FileIRs.
// Called once at index time — never at query time.
func NewGraphScorer(g *graph.Graph, fileIRs []*ir.FileIR) *GraphScorer {
	gs := &GraphScorer{
		scores:    make(map[string]*GraphScores),
		cooccur:   make(map[string]map[string]bool),
		siblingOf: make(map[string][]string),
	}

	gs.computePageRank(g)
	gs.computeCallerCount(g)
	gs.computeBetweenness(g)
	gs.computeCooccurrence(g, fileIRs)
	gs.computeSiblings(fileIRs)

	return gs
}

// Score returns the pre-computed GraphScores for a node.
// Returns zero scores if the node is not indexed.
func (gs *GraphScorer) Score(nodeID string) *GraphScores {
	if s, ok := gs.scores[nodeID]; ok {
		return s
	}
	return &GraphScores{}
}

// Cooccurs reports whether nodeA and nodeB co-occur in the codebase.
func (gs *GraphScorer) Cooccurs(nodeA, nodeB string) bool {
	if set, ok := gs.cooccur[nodeA]; ok {
		return set[nodeB]
	}
	return false
}

// TopCooccurring returns the top N node IDs that co-occur with nodeID,
// ordered by their PageRank score descending.
func (gs *GraphScorer) TopCooccurring(nodeID string, n int) []string {
	set, ok := gs.cooccur[nodeID]
	if !ok {
		return nil
	}

	type scored struct {
		id    string
		score float64
	}
	candidates := make([]scored, 0, len(set))
	for id := range set {
		pr := 0.0
		if s, ok := gs.scores[id]; ok {
			pr = s.PageRank
		}
		candidates = append(candidates, scored{id, pr})
	}

	// Sort by PageRank descending.
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].score > candidates[j-1].score; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}

	result := make([]string, 0, n)
	for i, c := range candidates {
		if i >= n {
			break
		}
		result = append(result, c.id)
	}
	return result
}

// SiblingsOf returns the sibling node IDs for a given node.
func (gs *GraphScorer) SiblingsOf(nodeID string) []string {
	return gs.siblingOf[nodeID]
}

// --- PageRank ---

// computePageRank runs the PageRank algorithm on the graph.
//
// PageRank identifies load-bearing symbols — the structs, interfaces,
// and functions that the rest of the codebase depends on.
//
// Parameters:
//
//	damping    = 0.85  (standard value)
//	iterations = 30    (sufficient for convergence on typical repos)
//
// External nodes are discounted by 0.1x after computation.
// "fmt" and "os" have high raw centrality but are irrelevant for
// code navigation — they're not your code.
func (gs *GraphScorer) computePageRank(g *graph.Graph) {
	const (
		damping          = 0.85
		iterations       = 30
		externalDiscount = 0.1
	)

	allNodes := g.AllNodes()
	n := float64(len(allNodes))
	if n == 0 {
		return
	}

	// Initialize all nodes with equal rank.
	ranks := make(map[string]float64, len(allNodes))
	for _, node := range allNodes {
		ranks[node.ID] = 1.0 / n
	}

	// Pre-compute out-degree for efficiency.
	outDegree := make(map[string]int, len(allNodes))
	for _, node := range allNodes {
		outDegree[node.ID] = len(g.OutEdges(node.ID))
	}

	// Iterate.
	newRanks := make(map[string]float64, len(allNodes))
	for iter := 0; iter < iterations; iter++ {
		for _, node := range allNodes {
			sum := 0.0
			for _, inEdge := range g.InEdges(node.ID) {
				od := outDegree[inEdge.From]
				if od > 0 {
					sum += ranks[inEdge.From] / float64(od)
				}
			}
			newRanks[node.ID] = (1-damping)/n + damping*sum
		}
		for k, v := range newRanks {
			ranks[k] = v
		}
	}

	// Normalize to [0, 1].
	maxRank := 0.0
	for _, r := range ranks {
		if r > maxRank {
			maxRank = r
		}
	}
	if maxRank == 0 {
		maxRank = 1
	}

	// Store in scores map, discounting externals.
	for _, node := range allNodes {
		normalized := ranks[node.ID] / maxRank
		if node.Kind == graph.NodeKindExternal {
			normalized *= externalDiscount
		}
		gs.ensureScore(node.ID).PageRank = normalized
	}
}

// --- Caller Count ---

// computeCallerCount normalizes the in-degree on call edges for each node.
// Nodes with many callers are more widely used and likely more important.
func (gs *GraphScorer) computeCallerCount(g *graph.Graph) {
	callerCounts := make(map[string]int)
	maxCount := 0

	for _, node := range g.AllNodes() {
		count := 0
		for _, edge := range g.InEdges(node.ID) {
			if edge.Kind == graph.EdgeKindCall {
				count++
			}
		}
		callerCounts[node.ID] = count
		if count > maxCount {
			maxCount = count
		}
	}

	if maxCount == 0 {
		maxCount = 1
	}

	for nodeID, count := range callerCounts {
		gs.ensureScore(nodeID).CallerCount = float64(count) / float64(maxCount)
	}
}

// --- Betweenness Centrality (approximation) ---

// computeBetweenness computes an approximation of betweenness centrality.
//
// Full betweenness centrality is O(n³) — too slow for large graphs.
// Approximation: (in_degree × out_degree) / total_edges
//
// Intuition: nodes that both receive many connections and send many
// connections are likely architectural bridges — they sit between
// subsystems and are important for understanding the codebase.
func (gs *GraphScorer) computeBetweenness(g *graph.Graph) {
	allNodes := g.AllNodes()
	totalEdges := float64(0)
	for _, node := range allNodes {
		totalEdges += float64(len(g.OutEdges(node.ID)))
	}
	if totalEdges == 0 {
		totalEdges = 1
	}

	maxBetweenness := 0.0
	betweennessRaw := make(map[string]float64, len(allNodes))

	for _, node := range allNodes {
		inDeg := float64(len(g.InEdges(node.ID)))
		outDeg := float64(len(g.OutEdges(node.ID)))
		b := (inDeg * outDeg) / totalEdges
		betweennessRaw[node.ID] = b
		if b > maxBetweenness {
			maxBetweenness = b
		}
	}

	if maxBetweenness == 0 {
		maxBetweenness = 1
	}

	for nodeID, b := range betweennessRaw {
		normalized := b / maxBetweenness
		// Apply log scaling to reduce dominance of super-hubs.
		if normalized > 0 {
			normalized = math.Log1p(normalized*9) / math.Log1p(10)
		}
		// Discount externals.
		node := g.Node(nodeID)
		if node != nil && node.Kind == graph.NodeKindExternal {
			normalized *= 0.1
		}
		gs.ensureScore(nodeID).Betweenness = normalized
	}
}

// --- Co-occurrence ---

// computeCooccurrence builds a node-level co-occurrence map.
//
// Two nodes co-occur if:
//   - They are in the same source file
//   - One calls the other (call edge)
//   - One defines the other (define edge)
//
// Used post-scoring for co-occurrence boost.
func (gs *GraphScorer) computeCooccurrence(g *graph.Graph, fileIRs []*ir.FileIR) {
	addCooccur := func(a, b string) {
		if a == b {
			return
		}
		if gs.cooccur[a] == nil {
			gs.cooccur[a] = make(map[string]bool)
		}
		if gs.cooccur[b] == nil {
			gs.cooccur[b] = make(map[string]bool)
		}
		gs.cooccur[a][b] = true
		gs.cooccur[b][a] = true
	}

	// Co-occur via same file.
	for _, file := range fileIRs {
		var nodeIDs []string
		nodeIDs = append(nodeIDs, file.Path)
		for _, fn := range file.Functions {
			nodeIDs = append(nodeIDs, fn.Qualified)
		}
		for _, sym := range file.Symbols {
			nodeIDs = append(nodeIDs, sym.Qualified)
		}
		for i, a := range nodeIDs {
			for _, b := range nodeIDs[i+1:] {
				addCooccur(a, b)
			}
		}
	}

	// Co-occur via call and define edges.
	for _, node := range g.AllNodes() {
		for _, edge := range g.OutEdges(node.ID) {
			if edge.Kind == graph.EdgeKindCall || edge.Kind == graph.EdgeKindDefine {
				addCooccur(edge.From, edge.To)
			}
		}
	}
}

// --- Siblings ---

// computeSiblings builds the sibling map from FileIRs.
// Siblings are other methods on the same receiver, or functions in the same file.
func (gs *GraphScorer) computeSiblings(fileIRs []*ir.FileIR) {
	for _, file := range fileIRs {
		byReceiver := make(map[string][]string)
		var plainFuncs []string

		for _, fn := range file.Functions {
			if fn.Receiver != "" {
				byReceiver[fn.Receiver] = append(byReceiver[fn.Receiver], fn.Qualified)
			} else {
				plainFuncs = append(plainFuncs, fn.Qualified)
			}
		}

		// Methods: siblings = other methods on same receiver.
		for _, fn := range file.Functions {
			if fn.Receiver == "" {
				continue
			}
			var sibs []string
			for _, q := range byReceiver[fn.Receiver] {
				if q != fn.Qualified {
					sibs = append(sibs, q)
				}
			}
			if len(sibs) > 0 {
				gs.siblingOf[fn.Qualified] = sibs
			}
		}

		// Plain functions: siblings = other plain functions in same file.
		for _, fn := range file.Functions {
			if fn.Receiver != "" {
				continue
			}
			var sibs []string
			for _, q := range plainFuncs {
				if q != fn.Qualified {
					sibs = append(sibs, q)
				}
			}
			if len(sibs) > 0 {
				gs.siblingOf[fn.Qualified] = sibs
			}
		}

		// Structs/interfaces: siblings = their methods.
		for _, sym := range file.Symbols {
			if methods, ok := byReceiver[sym.Name]; ok {
				gs.siblingOf[sym.Qualified] = methods
			}
		}
	}
}

// --- helpers ---

func (gs *GraphScorer) ensureScore(nodeID string) *GraphScores {
	if _, ok := gs.scores[nodeID]; !ok {
		gs.scores[nodeID] = &GraphScores{}
	}
	return gs.scores[nodeID]
}
