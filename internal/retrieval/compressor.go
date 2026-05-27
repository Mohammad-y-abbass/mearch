package retrieval

import (
	"path/filepath"
	"sort"

	"github.com/mohamamd-y-abbass/mearch/internal/graph"
)

// Token budget configuration.
const (
	DefaultTokenBudget = 4000 // default max tokens in output context

	// Approximate token cost per node type in the output.
	// Conservative estimates — better to under-fill than overflow.
	tokensPerFunction  = 80
	tokensPerStruct    = 60
	tokensPerFile      = 40
	tokensPerPackage   = 20
	tokensPerOther     = 50
	tokensPerCollapsed = 50 // collapsed file summary entry

	// fileCollapseThreshold: if a file contributes more than this many
	// nodes, collapse them into a single file-level summary entry.
	fileCollapseThreshold = 4
)

// Score thresholds for tiered compression.
const (
	tier2MinScore = 0.50 // include until 60% of budget
	tier3MinScore = 0.30 // include until 90% of budget
)

// ContextResult is the final output of the retrieval engine.
// Ready for direct consumption by an AI agent.
type ContextResult struct {
	// Nodes is the final ranked list of relevant nodes, highest score first.
	Nodes []RankedNode

	// CollapsedFiles maps filePath → []symbolNames for files where
	// many nodes were collapsed into a single file-level entry.
	// The agent should look at the whole file for these.
	CollapsedFiles map[string][]string

	// Files is the deduplicated list of source files containing result nodes.
	Files []string

	// Seeds are the seeds detected from the query.
	// Useful for debugging — shows what the engine latched onto.
	Seeds []Seed

	// ExpandedTokens are the query tokens after expansion.
	ExpandedTokens []string

	// TokenEstimate is the approximate token count of this result.
	TokenEstimate int

	// CandidateCount is how many nodes were in the pool before compression.
	CandidateCount int

	// Query is the original query string.
	Query string
}

// Compressor applies tiered compression to a ranked node list,
// enforcing a token budget and collapsing file clusters.
type Compressor struct {
	tokenBudget int
}

// NewCompressor constructs a Compressor with the given token budget.
func NewCompressor(tokenBudget int) *Compressor {
	if tokenBudget <= 0 {
		tokenBudget = DefaultTokenBudget
	}
	return &Compressor{tokenBudget: tokenBudget}
}

// Compress applies the token budget and returns the final node list
// along with any collapsed file summaries.
//
// Pipeline:
//  1. Hard filter — drop nodes that should never be in output
//  2. Tier 1 — always include: seeds + direct callers + exact name matches
//  3. Tier 2 — include until 60% budget: score >= 0.50, exported, depth <= 2
//  4. Tier 3 — include until 90% budget: score >= 0.30, depth <= 3
//  5. File collapse — if a file contributes >= fileCollapseThreshold nodes,
//     replace them with a single file summary entry
//  6. Reserve last 10% of budget for collapsed file entries
func (c *Compressor) Compress(
	ranked []RankedNode,
	seeds []Seed,
) ([]RankedNode, map[string][]string) {

	// Build seed ID set.
	seedIDs := make(map[string]bool, len(seeds))
	for _, s := range seeds {
		seedIDs[s.NodeID] = true
	}

	// --- Step 1: Hard filter ---
	filtered := c.hardFilter(ranked, seedIDs)

	// --- Step 2-4: Tiered selection ---
	selected := c.tieredSelect(filtered, seedIDs)

	// --- Step 5-6: File collapse ---
	result, collapsed := c.collapseFiles(selected)

	return result, collapsed
}

// hardFilter removes nodes that should never appear in the output.
//
// Rules:
//   - Always drop NodeKindExternal (AI already knows stdlib/third-party)
//   - Always drop score < 0.12
//   - Drop unexported nodes at depth >= 3 with score < 0.25
//   - Always keep seed nodes regardless of score
func (c *Compressor) hardFilter(ranked []RankedNode, seedIDs map[string]bool) []RankedNode {
	result := make([]RankedNode, 0, len(ranked))
	for _, rn := range ranked {
		// Seeds always survive.
		if seedIDs[rn.Node.ID] {
			result = append(result, rn)
			continue
		}

		// Drop external nodes entirely.
		if rn.Node.Kind == graph.NodeKindExternal {
			continue
		}

		// Drop very low confidence nodes.
		if rn.Score < 0.12 {
			continue
		}

		// Drop deep unexported nodes with low scores.
		if rn.Node.Visibility == graph.VisibilityUnexported &&
			rn.Depth >= 3 &&
			rn.Score < 0.25 {
			continue
		}

		result = append(result, rn)
	}
	return result
}

// tieredSelect fills the token budget in three tiers.
func (c *Compressor) tieredSelect(filtered []RankedNode, seedIDs map[string]bool) []RankedNode {
	included := make(map[string]bool)
	result := make([]RankedNode, 0, c.tokenBudget/tokensPerFunction)
	tokensSoFar := 0

	budget60 := int(float64(c.tokenBudget) * 0.60)
	budget90 := int(float64(c.tokenBudget) * 0.90)

	addNode := func(rn RankedNode) bool {
		if included[rn.Node.ID] {
			return false
		}
		cost := tokenCost(rn.Node)
		if tokensSoFar+cost > c.tokenBudget {
			return false
		}
		included[rn.Node.ID] = true
		result = append(result, rn)
		tokensSoFar += cost
		return true
	}

	// --- Tier 1: Always include ---
	// Seeds, direct backward-depth-1 nodes, exact name match nodes.
	for _, rn := range filtered {
		isSeed := seedIDs[rn.Node.ID]
		isDirectCaller := rn.Direction == "backward" && rn.Depth == 1
		isHighScore := rn.Score >= 0.85

		if isSeed || isDirectCaller || isHighScore {
			addNode(rn)
		}
	}

	// --- Tier 2: Score >= 0.50 until 60% budget ---
	for _, rn := range filtered {
		if tokensSoFar >= budget60 {
			break
		}
		if rn.Score >= tier2MinScore &&
			rn.Node.Visibility == graph.VisibilityExported &&
			rn.Depth <= 2 {
			addNode(rn)
		}
	}

	// --- Tier 3: Score >= 0.30 until 90% budget ---
	for _, rn := range filtered {
		if tokensSoFar >= budget90 {
			break
		}
		if rn.Score >= tier3MinScore && rn.Depth <= 3 {
			addNode(rn)
		}
	}

	return result
}

// collapseFiles replaces file clusters with summary entries.
//
// If a file contributes >= fileCollapseThreshold nodes to the result,
// those nodes are removed and replaced with a single entry in the
// CollapsedFiles map. The agent knows to look at the whole file.
//
// This saves significant tokens on large files with many relevant symbols.
func (c *Compressor) collapseFiles(selected []RankedNode) ([]RankedNode, map[string][]string) {
	// Count nodes per file.
	fileNodes := make(map[string][]RankedNode)
	for _, rn := range selected {
		if rn.Node.FilePath != "" {
			fileNodes[rn.Node.FilePath] = append(fileNodes[rn.Node.FilePath], rn)
		}
	}

	// Identify files to collapse.
	toCollapse := make(map[string]bool)
	for filePath, nodes := range fileNodes {
		if len(nodes) >= fileCollapseThreshold {
			toCollapse[filePath] = true
		}
	}

	if len(toCollapse) == 0 {
		return selected, nil
	}

	// Build collapsed file map.
	collapsed := make(map[string][]string)
	for filePath := range toCollapse {
		nodes := fileNodes[filePath]
		names := make([]string, 0, len(nodes))
		for _, rn := range nodes {
			names = append(names, rn.Node.Name)
		}
		sort.Strings(names)
		collapsed[filePath] = names
	}

	// Build result excluding collapsed nodes, keeping file-level nodes.
	result := make([]RankedNode, 0, len(selected))
	for _, rn := range selected {
		if toCollapse[rn.Node.FilePath] {
			// If this is the file node itself, keep it as the representative.
			if rn.Node.Kind == graph.NodeKindFile {
				result = append(result, rn)
			}
			// Individual symbol nodes from collapsed files are dropped.
			continue
		}
		result = append(result, rn)
	}

	// Add file-level nodes for collapsed files that don't have one yet.
	includedFiles := make(map[string]bool)
	for _, rn := range result {
		if rn.Node.Kind == graph.NodeKindFile {
			includedFiles[rn.Node.ID] = true
		}
	}

	for filePath := range toCollapse {
		if !includedFiles[filePath] {
			// Find the highest-scoring node from this file to use as score.
			bestScore := 0.0
			for _, rn := range fileNodes[filePath] {
				if rn.Score > bestScore {
					bestScore = rn.Score
				}
			}
			result = append(result, RankedNode{
				Node: &graph.Node{
					ID:       filePath,
					Kind:     graph.NodeKindFile,
					Name:     filepath.Base(filePath),
					FilePath: filePath,
				},
				Score:  bestScore,
				Reason: "collapsed file summary",
			})
		}
	}

	return result, collapsed
}

// tokenCost estimates the token cost of including a node in the output.
func tokenCost(node *graph.Node) int {
	switch node.Kind {
	case graph.NodeKindFunction, graph.NodeKindMethod:
		return tokensPerFunction
	case graph.NodeKindStruct, graph.NodeKindInterface:
		return tokensPerStruct
	case graph.NodeKindFile:
		return tokensPerFile
	case graph.NodeKindPackage:
		return tokensPerPackage
	default:
		return tokensPerOther
	}
}

// ExtractFiles returns a deduplicated, sorted list of file paths
// from a set of ranked nodes.
func ExtractFiles(nodes []RankedNode) []string {
	seen := make(map[string]bool)
	var files []string
	for _, rn := range nodes {
		fp := rn.Node.FilePath
		if fp == "" {
			fp = rn.Node.ID // file nodes use ID as path
		}
		if fp != "" && !seen[fp] && rn.Node.Kind == graph.NodeKindFile ||
			(rn.Node.FilePath != "" && !seen[rn.Node.FilePath]) {
			p := rn.Node.FilePath
			if p == "" {
				p = rn.Node.ID
			}
			if !seen[p] {
				seen[p] = true
				files = append(files, p)
			}
		}
	}
	sort.Strings(files)
	return files
}
