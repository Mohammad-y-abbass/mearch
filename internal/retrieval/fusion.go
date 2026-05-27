package retrieval

import (
	"fmt"
	"sort"

	"github.com/mohamamd-y-abbass/mearch/internal/graph"
)

// Fusion scoring weights — must sum to 1.0.
//
// Tuned for AI agent queries against source code:
//
//	BM25 (0.35):       text relevance — strongest signal for what the query asks
//	PathScore (0.25):  graph proximity to seed — strong structural signal
//	PageRank (0.15):   codebase-wide importance
//	CallerCount (0.10): usage frequency — widely-called = widely important
//	Betweenness (0.08): architectural bridging
//	Visibility (0.04): exported preferred
//	Sibling (0.03):    cluster coherence
const (
	wBM25        = 0.35
	wPathScore   = 0.25
	wPageRank    = 0.15
	wCallerCount = 0.10
	wBetweenness = 0.08
	wVisibility  = 0.04
	wSibling     = 0.03
)

// Co-occurrence boost applied post-scoring.
const cooccurBoost = 0.08

// RankedNode is a fully scored candidate node ready for compression.
type RankedNode struct {
	Node       *graph.Node
	Score      float64 // final fused score
	BM25Score  float64 // text relevance component
	GraphScore float64 // structural component
	Depth      int
	Direction  string // "seed", "forward", "backward"
	SeedID     string
	Reason     string // human-readable explanation
}

// Fusion combines BM25 scores and graph signal scores into a single
// ranked list of nodes, then applies co-occurrence boosting.
type Fusion struct {
	bm25   *BM25Index
	scorer *GraphScorer
	prep   *Preprocessor
}

// NewFusion constructs a Fusion.
func NewFusion(bm25 *BM25Index, scorer *GraphScorer) *Fusion {
	return &Fusion{
		bm25:   bm25,
		scorer: scorer,
		prep:   NewPreprocessor(),
	}
}

// Rank scores all candidates and returns them sorted by final score descending.
//
// Pipeline:
//  1. Score each candidate with weighted fusion of BM25 + graph signals
//  2. Apply co-occurrence boost
//  3. Sort descending
func (f *Fusion) Rank(
	candidates []Candidate,
	expandedTokens []string,
	seeds []Seed,
	hints *IntentHints,
) []RankedNode {
	if len(candidates) == 0 {
		return nil
	}

	// Build seed ID set for fast lookup.
	seedIDs := make(map[string]float64, len(seeds))
	for _, s := range seeds {
		seedIDs[s.NodeID] = s.Score
	}

	// Score all candidates.
	ranked := make([]RankedNode, 0, len(candidates))
	for _, c := range candidates {
		rn := f.scoreCandidate(c, expandedTokens, seedIDs, hints)
		ranked = append(ranked, rn)
	}

	// Sort by score descending before co-occurrence boost.
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].Score > ranked[j].Score
	})

	// Apply co-occurrence boost.
	ranked = f.applyCooccurrenceBoost(ranked)

	// Re-sort after boost.
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].Score > ranked[j].Score
	})

	return ranked
}

// scoreCandidate computes the fused score for a single candidate.
func (f *Fusion) scoreCandidate(
	c Candidate,
	tokens []string,
	seedIDs map[string]float64,
	hints *IntentHints,
) RankedNode {
	gs := f.scorer.Score(c.Node.ID)

	// --- Signal 1: BM25 text relevance ---
	bm25Score := f.bm25.Score(c.Node.ID, tokens)
	// Normalize BM25 score — it's already normalized by BM25Index.Search
	// but Score() returns raw values. Clamp to [0,1].
	bm25Score = clamp(bm25Score/10.0, 0, 1.0)

	// --- Signal 2: Path score (graph distance + edge weights) ---
	pathScore := c.PathScore

	// --- Signal 3: PageRank centrality ---
	pageRank := gs.PageRank

	// --- Signal 4: Caller count ---
	callerCount := gs.CallerCount

	// --- Signal 5: Betweenness ---
	betweenness := gs.Betweenness

	// --- Signal 6: Visibility ---
	visScore := 0.5
	if c.Node.Visibility == graph.VisibilityExported {
		visScore = 1.0
	}

	// --- Signal 7: Sibling relevance ---
	// If any sibling of this node is also a seed or high-scoring candidate,
	// this node gets a sibling relevance boost.
	siblingScore := 0.0
	for _, sibID := range f.scorer.SiblingsOf(c.Node.ID) {
		if seedScore, ok := seedIDs[sibID]; ok {
			siblingScore = maxFloat(siblingScore, seedScore*0.5)
		}
	}
	siblingScore = clamp(siblingScore, 0, 1.0)

	// --- Fused score ---
	graphComponent := (pathScore * wPathScore) +
		(pageRank * wPageRank) +
		(callerCount * wCallerCount) +
		(betweenness * wBetweenness) +
		(visScore * wVisibility) +
		(siblingScore * wSibling)

	textComponent := bm25Score * wBM25

	finalScore := textComponent + graphComponent

	// --- Intent-based adjustments ---
	if hints != nil {
		if hints.PreferBackward && c.Direction == "backward" {
			finalScore = clamp(finalScore*1.15, 0, 1.0)
		}
		if hints.PreferForward && c.Direction == "forward" {
			finalScore = clamp(finalScore*1.10, 0, 1.0)
		}
		if hints.PreferKind != "" && c.Node.Kind == hints.PreferKind {
			finalScore = clamp(finalScore+0.08, 0, 1.0)
		}
	}

	// Seeds always score at least their seed score.
	if seedScore, ok := seedIDs[c.Node.ID]; ok {
		finalScore = maxFloat(finalScore, seedScore*0.90)
	}

	finalScore = clamp(finalScore, 0, 1.0)

	reason := fmt.Sprintf(
		"bm25=%.2f path=%.2f pr=%.2f callers=%.2f between=%.2f vis=%.2f sib=%.2f → %.2f [%s]",
		bm25Score, pathScore, pageRank, callerCount, betweenness,
		visScore, siblingScore, finalScore, c.Direction,
	)

	return RankedNode{
		Node:       c.Node,
		Score:      finalScore,
		BM25Score:  bm25Score,
		GraphScore: graphComponent,
		Depth:      c.Depth,
		Direction:  c.Direction,
		SeedID:     c.SeedID,
		Reason:     reason,
	}
}

// applyCooccurrenceBoost boosts nodes that co-occur with high-scoring nodes.
//
// If node A scores >= 0.65 and node B co-occurs with A in the codebase,
// node B gets a small boost. This surfaces feature clusters where related
// symbols aren't directly connected in the graph but always appear together.
func (f *Fusion) applyCooccurrenceBoost(ranked []RankedNode) []RankedNode {
	// Build a score lookup for quick access.
	scoreMap := make(map[string]float64, len(ranked))
	for _, rn := range ranked {
		scoreMap[rn.Node.ID] = rn.Score
	}

	for i, rn := range ranked {
		if rn.Score < 0.65 {
			continue // only high-scoring nodes trigger boosts
		}

		// Find co-occurring nodes and boost them.
		for _, coID := range f.scorer.TopCooccurring(rn.Node.ID, 5) {
			if _, exists := scoreMap[coID]; !exists {
				continue
			}
			// Find and boost the co-occurring node in the ranked slice.
			for j := range ranked {
				if ranked[j].Node.ID == coID {
					ranked[j].Score = clamp(ranked[j].Score+cooccurBoost, 0, 1.0)
					ranked[j].Reason += " +cooccur"
					break
				}
			}
		}
		_ = i
	}

	return ranked
}
