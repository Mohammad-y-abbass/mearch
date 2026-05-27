package retrieval

import (
	"sort"
	"strings"

	"github.com/mohamamd-y-abbass/mearch/internal/graph"
)

// maxExpansionTerms is the maximum number of terms added by expansion.
// Too many expansion terms dilute the original query signal.
const maxExpansionTerms = 8

// cooccurrenceTopN is how many co-occurring terms to inject per query term.
const cooccurrenceTopN = 3

// codeSynonyms maps stemmed programming terms to their stemmed synonyms.
//
// Hand-curated for programming vocabulary. Only terms that are genuinely
// interchangeable in code contexts are listed here.
//
// All terms are pre-stemmed to match the preprocessor output.
// Update when new patterns emerge from retrieval quality analysis.
var codeSynonyms = map[string][]string{
	// File/path operations
	"ignor":   {"filter", "exclud", "skip", "block", "deny", "reject"},
	"filter":  {"ignor", "exclud", "skip", "prune"},
	"skip":    {"ignor", "filter", "exclud", "bypass"},
	"walk":    {"travers", "scan", "crawl", "iter", "visit"},
	"travers": {"walk", "scan", "crawl", "iter"},
	"scan":    {"walk", "travers", "crawl", "discov", "find"},
	"discov":  {"scan", "walk", "find", "detect", "search"},

	// Parsing
	"pars":    {"lex", "tokeniz", "syntax", "analyz"},
	"extract": {"pars", "read", "retriev", "get"},
	"tokeniz": {"pars", "lex", "split"},

	// Graph/structure
	"graph":  {"node", "edge", "tree", "network"},
	"node":   {"vertex", "element", "symbol"},
	"edge":   {"link", "connect", "relat"},
	"depend": {"import", "requir", "use"},
	"import": {"depend", "requir", "includ"},

	// Search/retrieval
	"retriev": {"search", "query", "lookup", "fetch", "find"},
	"search":  {"retriev", "query", "lookup", "find"},
	"query":   {"search", "retriev", "lookup", "request"},
	"index":   {"catalog", "map", "registr", "lookup"},

	// Code symbols
	"symbol":   {"ident", "name", "token", "defin"},
	"function": {"func", "method", "procedur", "routin"},
	"method":   {"function", "func", "procedur"},
	"struct":   {"type", "class", "record", "object"},
	"interfac": {"contract", "abstract", "protocol"},

	// Build/compile
	"build":   {"compil", "generat", "construct"},
	"compil":  {"build", "generat"},
	"generat": {"build", "creat", "produc"},

	// Error handling
	"error":  {"err", "except", "fail", "fault"},
	"fail":   {"error", "err", "except"},
	"handle": {"manag", "process", "deal"},

	// Config
	"config": {"option", "set", "prefer", "param", "arg"},
	"option": {"config", "set", "prefer", "param"},
	"param":  {"arg", "input", "config", "option"},

	// Storage
	"stor":    {"sav", "persist", "cach", "write"},
	"cach":    {"stor", "buffer", "memoiz"},
	"persist": {"stor", "sav", "write"},

	// IR specific
	"ir":     {"intermedi", "represent", "semant"},
	"semant": {"mean", "ir", "abstract"},
	"ast":    {"syntax", "tree", "pars"},
}

// Expander expands a query token set with related terms.
//
// Expansion dramatically improves recall for queries where the exact
// words don't appear in symbol names or comments. The key insight:
// the codebase itself teaches the engine what terms co-occur, which
// is more reliable than any external dictionary.
//
// Three expansion strategies:
//  1. Co-occurrence: terms that always appear near query terms in this codebase
//  2. Synonyms: hand-curated programming vocabulary synonyms
//  3. Graph neighbor injection: tokens from graph-adjacent nodes
type Expander struct {
	g            *graph.Graph
	bm25         *BM25Index
	prep         *Preprocessor
	cooccurrence map[string]map[string]int // token → {coToken: count}
}

// NewExpander constructs an Expander.
//
// cooccurrence is built at index time from the document corpus.
func NewExpander(g *graph.Graph, bm25 *BM25Index, cooccurrence map[string]map[string]int) *Expander {
	return &Expander{
		g:            g,
		bm25:         bm25,
		prep:         NewPreprocessor(),
		cooccurrence: cooccurrence,
	}
}

// Expand takes stemmed query tokens and returns an expanded token set.
//
// The expanded set always includes the original tokens plus additional
// related terms. Original tokens are never removed.
//
// Expansion is limited to maxExpansionTerms new terms to prevent
// query drift — too many expansion terms dilute the original signal.
func (e *Expander) Expand(tokens []string) []string {
	if len(tokens) == 0 {
		return tokens
	}

	// Track original tokens and new additions separately.
	original := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		original[t] = true
	}

	type candidate struct {
		token string
		score float64
	}
	var candidates []candidate
	seen := make(map[string]bool)
	for _, t := range tokens {
		seen[t] = true
	}

	// --- Strategy 1: Co-occurrence expansion ---
	// Terms that frequently appear alongside query terms in this codebase.
	// This is the most valuable expansion because it uses corpus-specific knowledge.
	for _, tok := range tokens {
		if coTerms, ok := e.cooccurrence[tok]; ok {
			// Sort co-occurring terms by frequency descending.
			type coTerm struct {
				term  string
				count int
			}
			var sorted []coTerm
			for term, count := range coTerms {
				if !seen[term] && !original[term] {
					sorted = append(sorted, coTerm{term, count})
				}
			}
			sort.Slice(sorted, func(i, j int) bool {
				return sorted[i].count > sorted[j].count
			})

			// Take top N, score proportional to co-occurrence frequency.
			maxCount := 1
			if len(sorted) > 0 {
				maxCount = sorted[0].count
			}
			for i, ct := range sorted {
				if i >= cooccurrenceTopN {
					break
				}
				score := float64(ct.count) / float64(maxCount) * 0.85
				candidates = append(candidates, candidate{ct.term, score})
				seen[ct.term] = true
			}
		}
	}

	// --- Strategy 2: Synonym expansion ---
	// Hand-curated programming vocabulary synonyms.
	for _, tok := range tokens {
		if synonyms, ok := codeSynonyms[tok]; ok {
			for _, syn := range synonyms {
				if !seen[syn] {
					candidates = append(candidates, candidate{syn, 0.70})
					seen[syn] = true
				}
			}
		}
	}

	// --- Strategy 3: Graph neighbor token injection ---
	// Find nodes matching the original query tokens via the BM25 index.
	// Inject tokens from their graph neighbors into the expanded query.
	// This uses graph structure to expand the query vocabulary.
	injected := 0
	for _, tok := range tokens {
		if injected >= 3 {
			break
		}
		// Find nodes matching this token.
		matchingNodes := e.bm25.LookupField(FieldName_Name, tok)
		if len(matchingNodes) == 0 {
			matchingNodes = e.bm25.Lookup(tok)
		}

		for _, nodeID := range matchingNodes {
			if injected >= 3 {
				break
			}
			node := e.g.Node(nodeID)
			if node == nil || node.Kind == graph.NodeKindExternal {
				continue
			}

			// Get this node's graph neighbors.
			neighbors := e.g.Neighbors(nodeID)
			for _, neighbor := range neighbors {
				if neighbor.Kind == graph.NodeKindExternal {
					continue
				}
				// Inject the neighbor's name tokens.
				neighborTokens := e.prep.ProcessSymbol(neighbor.Name)
				for _, nt := range neighborTokens {
					if !seen[nt] && !stopWords[nt] {
						candidates = append(candidates, candidate{nt, 0.50})
						seen[nt] = true
						injected++
					}
				}
			}
		}
	}

	// Sort candidates by score descending.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	// Build expanded token list: originals first, then top expansions.
	expanded := make([]string, len(tokens), len(tokens)+maxExpansionTerms)
	copy(expanded, tokens)

	added := 0
	for _, c := range candidates {
		if added >= maxExpansionTerms {
			break
		}
		expanded = append(expanded, c.token)
		added++
	}

	return expanded
}

// BuildCooccurrence builds a token co-occurrence matrix from the document corpus.
//
// Two tokens co-occur if they appear in the same document.
// The count tracks how many documents they co-occur in.
//
// This is called once at index time.
// Result is passed to NewExpander.
func BuildCooccurrence(docs map[string]*Document) map[string]map[string]int {
	cooccurrence := make(map[string]map[string]int)

	for _, doc := range docs {
		// Collect all tokens in this document across all fields.
		var allTokens []string
		seen := make(map[string]bool)
		for _, tokens := range doc.Fields {
			for _, tok := range tokens {
				if !seen[tok] {
					seen[tok] = true
					allTokens = append(allTokens, tok)
				}
			}
		}

		// All pairs of tokens in this document co-occur.
		// Only track pairs where both tokens are non-trivial.
		for i, a := range allTokens {
			if len(a) < 3 || stopWords[a] {
				continue
			}
			for _, b := range allTokens[i+1:] {
				if len(b) < 3 || stopWords[b] || a == b {
					continue
				}
				// Symmetric co-occurrence.
				if cooccurrence[a] == nil {
					cooccurrence[a] = make(map[string]int)
				}
				if cooccurrence[b] == nil {
					cooccurrence[b] = make(map[string]int)
				}
				cooccurrence[a][b]++
				cooccurrence[b][a]++
			}
		}
	}

	return cooccurrence
}

// keep strings import used in synonym handling
var _ = strings.Join
