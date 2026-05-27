package retrieval

import (
	"github.com/mohamamd-y-abbass/mearch/internal/graph"
	"github.com/mohamamd-y-abbass/mearch/internal/ir"
)

// Engine is the retrieval engine entry point.
//
// It wires all pipeline stages together and exposes a single Query() method.
// All heavy computation (BM25 index, PageRank, co-occurrence) happens at
// index time via Build(). Query() is fast — just lookups and traversal.
//
// Engine is safe for concurrent use after Build() returns.
//
// Usage:
//
//	engine := retrieval.Build(graph, fileIRs, retrieval.DefaultTokenBudget)
//	result := engine.Query("fix scanner ignore rules")
type Engine struct {
	g          *graph.Graph
	bm25       *BM25Index
	scorer     *GraphScorer
	expander   *Expander
	seeds      *SeedDetector
	traverser  *Traverser
	fusion     *Fusion
	compressor *Compressor
	prep       *Preprocessor
}

// Build constructs a fully initialized Engine from the graph and FileIRs.
//
// Build is the only way to create an Engine. It runs all index-time
// computation in order:
//
//  1. Build enriched documents from graph + FileIRs
//  2. Build BM25 index from documents
//  3. Build co-occurrence matrix from documents
//  4. Compute graph scores (PageRank, betweenness, caller count)
//  5. Wire all pipeline stages together
//
// Build may take a few seconds on large repos — this is expected and
// acceptable since it runs once at index time, not per query.
func Build(g *graph.Graph, fileIRs []*ir.FileIR, tokenBudget int) *Engine {
	// Step 1: Build enriched documents.
	docBuilder := NewDocumentBuilder()
	docs := docBuilder.BuildAll(g, fileIRs)

	// Step 2: Build BM25 index.
	bm25 := BuildBM25Index(docs)

	// Step 3: Build co-occurrence matrix from documents.
	cooccurrence := BuildCooccurrence(docs)

	// Step 4: Compute graph scores.
	scorer := NewGraphScorer(g, fileIRs)

	// Step 5: Wire pipeline stages.
	expander := NewExpander(g, bm25, cooccurrence)
	seedDetector := NewSeedDetector(g, bm25)
	traverser := NewTraverser(g)
	fusion := NewFusion(bm25, scorer)
	compressor := NewCompressor(tokenBudget)

	return &Engine{
		g:          g,
		bm25:       bm25,
		scorer:     scorer,
		expander:   expander,
		seeds:      seedDetector,
		traverser:  traverser,
		fusion:     fusion,
		compressor: compressor,
		prep:       NewPreprocessor(),
	}
}

// Query runs the full retrieval pipeline for a natural language query.
//
// Pipeline:
//  1. Preprocess query → stemmed tokens
//  2. Expand tokens → co-occurrence + synonyms + graph neighbor injection
//  3. Detect seeds → multi-strategy (BM25 name, comment, full, fuzzy, intent)
//  4. Traverse graph → bidirectional weighted BFS with intent hints
//  5. Fuse scores → BM25 + graph signals + co-occurrence boost
//  6. Compress → tiered token budget + file collapse
//  7. Return ContextResult
//
// Query is safe to call concurrently.
func (e *Engine) Query(query string) *ContextResult {
	result := &ContextResult{Query: query}

	// Step 1: Preprocess.
	originalTokens := e.prep.ProcessQuery(query)
	if len(originalTokens) == 0 {
		return result
	}

	// Step 2: Expand.
	expandedTokens := e.expander.Expand(originalTokens)
	result.ExpandedTokens = expandedTokens

	// Step 3: Detect seeds.
	seeds, hints := e.seeds.Detect(originalTokens, expandedTokens)
	result.Seeds = seeds

	if len(seeds) == 0 {
		return result
	}

	// Step 4: Traverse.
	candidates := e.traverser.Traverse(seeds, hints)
	result.CandidateCount = len(candidates)

	if len(candidates) == 0 {
		return result
	}

	// Step 5: Fuse scores.
	ranked := e.fusion.Rank(candidates, expandedTokens, seeds, hints)

	// Step 6: Compress.
	compressed, collapsed := e.compressor.Compress(ranked, seeds)
	result.Nodes = compressed
	result.CollapsedFiles = collapsed
	result.Files = ExtractFiles(compressed)

	// Estimate token count.
	total := 0
	for _, rn := range compressed {
		total += tokenCost(rn.Node)
	}
	for range collapsed {
		total += tokensPerCollapsed
	}
	result.TokenEstimate = total

	return result
}

// Stats returns a summary of the index built for this engine.
// Useful for debugging and logging.
func (e *Engine) Stats() EngineStats {
	graphStats := e.g.Stats()
	return EngineStats{
		GraphNodes: graphStats.TotalNodes,
		GraphEdges: graphStats.TotalEdges,
	}
}

// EngineStats holds summary statistics about the engine's index.
type EngineStats struct {
	GraphNodes int
	GraphEdges int
}
