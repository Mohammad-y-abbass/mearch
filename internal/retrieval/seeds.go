package retrieval

import (
	"sort"
	"strings"

	"github.com/mohamamd-y-abbass/mearch/internal/graph"
)

// Seed detection configuration.
const (
	maxSeeds        = 10   // max seeds fed into traversal
	minSeedScore    = 0.12 // seeds below this are dropped
	maxFuzzyDist    = 2    // max Levenshtein distance for fuzzy matching
	multiStratBonus = 0.08 // bonus when a node is found by 2+ strategies
	triStratBonus   = 0.15 // bonus when a node is found by 3+ strategies
)

// Seed is a candidate starting node for graph traversal.
// Detected from the query using multiple strategies.
type Seed struct {
	NodeID     string   // graph node ID
	Score      float64  // confidence score 0.0–1.0
	Strategies []string // which strategies detected this seed
}

// SeedDetector runs multiple seed detection strategies and merges results.
//
// Each strategy has different strengths and failure modes.
// Running all strategies and taking the union ensures no single
// failure mode breaks retrieval.
type SeedDetector struct {
	g    *graph.Graph
	bm25 *BM25Index
	prep *Preprocessor
}

// NewSeedDetector constructs a SeedDetector.
func NewSeedDetector(g *graph.Graph, bm25 *BM25Index) *SeedDetector {
	return &SeedDetector{g: g, bm25: bm25, prep: NewPreprocessor()}
}

// Detect runs all seed detection strategies against the expanded query tokens
// and returns a ranked, deduplicated list of seed nodes.
//
// Strategies (ordered by typical precision):
//
//	A: BM25 name field search      — exact/stemmed name match, highest precision
//	B: BM25 comment field search   — docstring vocabulary matching
//	C: BM25 full corpus search     — all fields, catches broader matches
//	D: Fuzzy name matching         — edit distance, catches typos
//	E: Structural intent matching  — query structure → graph traversal hints
func (sd *SeedDetector) Detect(originalTokens, expandedTokens []string) ([]Seed, *IntentHints) {
	// scores accumulates the best score per node across all strategies.
	type seedEntry struct {
		score      float64
		strategies []string
	}
	scores := make(map[string]*seedEntry)

	addSeed := func(nodeID string, score float64, strategy string) {
		if e, ok := scores[nodeID]; ok {
			if score > e.score {
				e.score = score
			}
			e.strategies = append(e.strategies, strategy)
		} else {
			scores[nodeID] = &seedEntry{score: score, strategies: []string{strategy}}
		}
	}

	// --- Strategy A: BM25 name field search ---
	// Highest precision — query word in symbol name is almost never wrong.
	nameResults := sd.bm25.Search(originalTokens, 20)
	for _, r := range nameResults {
		node := sd.g.Node(r.NodeID)
		if node == nil || node.Kind == graph.NodeKindExternal {
			continue
		}
		addSeed(r.NodeID, r.Score*0.95, "bm25-name")
	}

	// --- Strategy B: BM25 comment/callers/callees field search ---
	// Catches vocabulary in docstrings and usage context.
	commentResults := sd.bm25.Search(originalTokens, 20)
	for _, r := range commentResults {
		node := sd.g.Node(r.NodeID)
		if node == nil || node.Kind == graph.NodeKindExternal {
			continue
		}
		addSeed(r.NodeID, r.Score*0.80, "bm25-comment")
	}

	// --- Strategy C: BM25 full corpus search on expanded tokens ---
	// Uses the expanded query — catches matches that only exist after
	// synonym or co-occurrence expansion.
	if len(expandedTokens) > len(originalTokens) {
		expandedResults := sd.bm25.Search(expandedTokens, 30)
		for _, r := range expandedResults {
			node := sd.g.Node(r.NodeID)
			if node == nil || node.Kind == graph.NodeKindExternal {
				continue
			}
			// Slightly lower confidence than original token matches.
			addSeed(r.NodeID, r.Score*0.70, "bm25-expanded")
		}
	}

	// --- Strategy D: Fuzzy name matching ---
	// Edit distance matching against symbol names.
	// Catches typos and near-misses not caught by stemming.
	// Only run on original tokens to avoid expanding fuzzy errors.
	for _, node := range sd.g.AllNodes() {
		if node.Kind == graph.NodeKindExternal {
			continue
		}
		nodeNameTokens := sd.prep.ProcessSymbol(node.Name)
		bestDist := maxFuzzyDist + 1

		for _, qt := range originalTokens {
			if len(qt) < 3 {
				continue // too short for fuzzy — too many false positives
			}
			for _, nt := range nodeNameTokens {
				if qt == nt {
					continue // exact match — already caught by BM25
				}
				dist := LevenshteinDistance(qt, nt)
				if dist < bestDist {
					bestDist = dist
				}
			}
		}

		if bestDist <= maxFuzzyDist {
			// Score decreases with distance: dist1=0.55, dist2=0.30
			fuzzyScore := 0.55 / float64(bestDist)
			addSeed(node.ID, fuzzyScore, "fuzzy")
		}
	}

	// --- Strategy E: Structural intent detection ---
	// Detect query patterns that map to graph structure.
	// Returns hints that influence traversal direction.
	hints := detectIntentHints(originalTokens, expandedTokens)

	// If intent hints suggest a specific node kind, boost matching nodes.
	if hints.PreferKind != "" {
		for nodeID, entry := range scores {
			node := sd.g.Node(nodeID)
			if node != nil && node.Kind == hints.PreferKind {
				entry.score = clamp(entry.score+0.10, 0, 1.0)
				entry.strategies = append(entry.strategies, "intent-boost")
			}
		}
	}

	// Apply multi-strategy bonuses and convert to Seed slice.
	seeds := make([]Seed, 0, len(scores))
	for nodeID, entry := range scores {
		score := entry.score

		// Bonus for being found by multiple strategies — high confidence.
		stratCount := len(uniqueStrings(entry.strategies))
		if stratCount >= 3 {
			score = clamp(score+triStratBonus, 0, 1.0)
		} else if stratCount >= 2 {
			score = clamp(score+multiStratBonus, 0, 1.0)
		}

		if score < minSeedScore {
			continue
		}

		seeds = append(seeds, Seed{
			NodeID:     nodeID,
			Score:      score,
			Strategies: entry.strategies,
		})
	}

	// Sort by score descending.
	sort.Slice(seeds, func(i, j int) bool {
		return seeds[i].Score > seeds[j].Score
	})

	// Trim to maxSeeds.
	if len(seeds) > maxSeeds {
		seeds = seeds[:maxSeeds]
	}

	return seeds, hints
}

// IntentHints are extracted from the query structure and used to
// influence traversal direction and scoring.
type IntentHints struct {
	// PreferBackward means the query is asking about callers/dependents.
	// e.g. "who calls X", "what uses X", "find callers of X"
	PreferBackward bool

	// PreferForward means the query is asking about dependencies.
	// e.g. "what does X depend on", "find dependencies of X"
	PreferForward bool

	// PreferKind is a node kind that should be boosted.
	// e.g. "find the interface for X" → prefer NodeKindInterface
	PreferKind graph.NodeKind

	// IsCallerQuery means the query is specifically about who calls something.
	IsCallerQuery bool

	// IsImportQuery means the query is about imports/dependencies.
	IsImportQuery bool
}

// detectIntentHints analyzes query tokens for structural patterns.
func detectIntentHints(originalTokens, expandedTokens []string) *IntentHints {
	hints := &IntentHints{}

	// Join tokens for simple substring checks.
	tokenStr := strings.Join(originalTokens, " ")

	// Caller/dependent queries.
	callerPatterns := []string{"caller", "call", "who", "user", "depend", "import"}
	for _, pat := range callerPatterns {
		if strings.Contains(tokenStr, pat) {
			hints.PreferBackward = true
			if strings.Contains(tokenStr, "call") {
				hints.IsCallerQuery = true
			}
			if strings.Contains(tokenStr, "import") || strings.Contains(tokenStr, "depend") {
				hints.IsImportQuery = true
			}
			break
		}
	}

	// Dependency queries.
	depPatterns := []string{"dep", "requir", "need", "use"}
	if !hints.PreferBackward {
		for _, pat := range depPatterns {
			if strings.Contains(tokenStr, pat) {
				hints.PreferForward = true
				break
			}
		}
	}

	// Node kind preferences.
	if strings.Contains(tokenStr, "interfac") {
		hints.PreferKind = graph.NodeKindInterface
	} else if strings.Contains(tokenStr, "struct") || strings.Contains(tokenStr, "type") {
		hints.PreferKind = graph.NodeKindStruct
	} else if strings.Contains(tokenStr, "function") || strings.Contains(tokenStr, "func") || strings.Contains(tokenStr, "method") {
		hints.PreferKind = graph.NodeKindFunction
	} else if strings.Contains(tokenStr, "packag") {
		hints.PreferKind = graph.NodeKindPackage
	}

	return hints
}

// uniqueStrings deduplicates a string slice preserving order.
func uniqueStrings(ss []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}
