package retrieval

import (
	"math"
	"sort"
)

// BM25 parameters.
// These are the standard values used in information retrieval literature
// and work well for code search without tuning.
//
//	k1 = 1.5  term frequency saturation point
//	           at k1=1.5, a term appearing 3x scores ~2x a term appearing 1x
//	           (not 3x — BM25 saturates TF to prevent runaway scores)
//
//	b  = 0.75 length normalization strength
//	           at b=0.75, a document 2x the average length is penalized
//	           but not completely discounted
const (
	bm25K1 = 1.5
	bm25B  = 0.75
)

// BM25Index is a field-weighted BM25 index over enriched Documents.
//
// Standard BM25 treats each document as a single bag of words.
// Field-weighted BM25 applies different weights to different fields,
// so a query term appearing in a symbol's name scores much higher
// than the same term appearing in its import list.
//
// This is the same technique used by Elasticsearch's multi-field search
// and is one of the biggest practical quality improvements over basic BM25.
//
// Index is read-only after Build(). Safe for concurrent use.
type BM25Index struct {
	// docs is the full document corpus.
	docs map[string]*Document

	// invertedIndex maps token → []nodeID that contain that token.
	// Broken down by field for field-weighted scoring.
	// invertedIndex[field][token] = []nodeID
	invertedIndex map[FieldName]map[string][]string

	// df maps field → token → document frequency (number of docs containing token).
	// Used for IDF calculation.
	df map[FieldName]map[string]int

	// avgFieldLen maps field → average token count across all documents.
	// Used for BM25 length normalization per field.
	avgFieldLen map[FieldName]float64

	// totalDocs is the total number of documents in the corpus.
	totalDocs int
}

// BM25Result is a single scored result from a BM25 search.
type BM25Result struct {
	NodeID string
	Score  float64
}

// BuildBM25Index constructs a BM25Index from a set of enriched Documents.
//
// This is called once at index time after documents are built.
// Building the index involves:
//  1. Computing document frequency per token per field
//  2. Computing average field length per field
//  3. Building inverted index per field
func BuildBM25Index(docs map[string]*Document) *BM25Index {
	idx := &BM25Index{
		docs:          docs,
		invertedIndex: make(map[FieldName]map[string][]string),
		df:            make(map[FieldName]map[string]int),
		avgFieldLen:   make(map[FieldName]float64),
		totalDocs:     len(docs),
	}

	// Initialize maps for each field.
	for field := range fieldWeights {
		idx.invertedIndex[field] = make(map[string][]string)
		idx.df[field] = make(map[string]int)
	}

	// Field length accumulators for average computation.
	fieldLenSum := make(map[FieldName]float64)
	fieldDocCount := make(map[FieldName]int)

	// Single pass over all documents.
	for nodeID, doc := range docs {
		for field, tokens := range doc.Fields {
			if len(tokens) == 0 {
				continue
			}

			// Accumulate field length.
			fieldLenSum[field] += float64(len(tokens))
			fieldDocCount[field]++

			// Build inverted index and document frequency.
			// Track which tokens we've seen in this doc to avoid
			// double-counting for DF (DF counts documents, not occurrences).
			seenInDoc := make(map[string]bool)
			for _, tok := range tokens {
				// Inverted index — all occurrences.
				idx.invertedIndex[field][tok] = appendUniqueStr(
					idx.invertedIndex[field][tok], nodeID,
				)
				// DF — count each doc once per token.
				if !seenInDoc[tok] {
					idx.df[field][tok]++
					seenInDoc[tok] = true
				}
			}
		}
	}

	// Compute average field lengths.
	for field := range fieldWeights {
		if fieldDocCount[field] > 0 {
			idx.avgFieldLen[field] = fieldLenSum[field] / float64(fieldDocCount[field])
		} else {
			idx.avgFieldLen[field] = 1.0
		}
	}

	return idx
}

// Search runs a BM25 query against the index and returns results
// sorted by score descending.
//
// tokens should be preprocessed (stemmed, stop words removed) — the same
// preprocessing applied to documents at index time.
//
// topK limits results. Pass 0 for all results.
func (idx *BM25Index) Search(tokens []string, topK int) []BM25Result {
	if len(tokens) == 0 || idx.totalDocs == 0 {
		return nil
	}

	scores := make(map[string]float64)

	// For each query token, score all matching documents.
	for _, tok := range tokens {
		for field, weight := range fieldWeights {
			idf := idx.idf(field, tok)
			if idf <= 0 {
				continue
			}

			// Find all documents containing this token in this field.
			matchingDocs := idx.invertedIndex[field][tok]
			for _, nodeID := range matchingDocs {
				doc, ok := idx.docs[nodeID]
				if !ok {
					continue
				}

				tf := termFrequency(tok, doc.Fields[field])
				fieldLen := float64(len(doc.Fields[field]))
				avgLen := idx.avgFieldLen[field]

				// BM25 field score.
				bm25Score := idx.bm25Term(tf, idf, fieldLen, avgLen)

				// Apply field weight.
				scores[nodeID] += bm25Score * weight
			}
		}
	}

	// Convert to sorted slice.
	results := make([]BM25Result, 0, len(scores))
	for nodeID, score := range scores {
		if score > 0 {
			results = append(results, BM25Result{NodeID: nodeID, Score: score})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	// Normalize scores to [0, 1] based on max score in results.
	if len(results) > 0 {
		maxScore := results[0].Score
		if maxScore > 0 {
			for i := range results {
				results[i].Score = results[i].Score / maxScore
			}
		}
	}

	if topK > 0 && len(results) > topK {
		results = results[:topK]
	}

	return results
}

// Score returns the BM25 score for a specific document against query tokens.
// Used during candidate scoring in the fusion step.
func (idx *BM25Index) Score(nodeID string, tokens []string) float64 {
	doc, ok := idx.docs[nodeID]
	if !ok || len(tokens) == 0 {
		return 0
	}

	total := 0.0
	for _, tok := range tokens {
		for field, weight := range fieldWeights {
			idf := idx.idf(field, tok)
			if idf <= 0 {
				continue
			}
			tf := termFrequency(tok, doc.Fields[field])
			if tf == 0 {
				continue
			}
			fieldLen := float64(len(doc.Fields[field]))
			avgLen := idx.avgFieldLen[field]
			total += idx.bm25Term(tf, idf, fieldLen, avgLen) * weight
		}
	}

	return total
}

// Lookup returns node IDs that contain the given token in any field.
// Used by seed detection for fast word index lookups.
func (idx *BM25Index) Lookup(token string) []string {
	seen := make(map[string]bool)
	var result []string
	for field := range fieldWeights {
		for _, nodeID := range idx.invertedIndex[field][token] {
			if !seen[nodeID] {
				seen[nodeID] = true
				result = append(result, nodeID)
			}
		}
	}
	return result
}

// LookupField returns node IDs that contain the token in a specific field.
func (idx *BM25Index) LookupField(field FieldName, token string) []string {
	return idx.invertedIndex[field][token]
}

// --- BM25 math ---

// idf computes the Inverse Document Frequency for a token in a field.
//
// Formula: log((N - df + 0.5) / (df + 0.5) + 1)
//
// This is the Robertson-Sparck Jones IDF variant used in BM25.
// The +1 ensures IDF is always positive even for very common terms.
func (idx *BM25Index) idf(field FieldName, token string) float64 {
	df := float64(idx.df[field][token])
	n := float64(idx.totalDocs)
	return math.Log((n-df+0.5)/(df+0.5) + 1)
}

// bm25Term computes the BM25 score contribution for a single term.
//
// Formula: IDF × (TF × (k1+1)) / (TF + k1×(1-b+b×fieldLen/avgLen))
func (idx *BM25Index) bm25Term(tf, idf, fieldLen, avgLen float64) float64 {
	if avgLen == 0 {
		avgLen = 1
	}
	numerator := tf * (bm25K1 + 1)
	denominator := tf + bm25K1*(1-bm25B+bm25B*(fieldLen/avgLen))
	return idf * (numerator / denominator)
}

// termFrequency counts how many times token appears in tokens slice.
func termFrequency(token string, tokens []string) float64 {
	count := 0
	for _, t := range tokens {
		if t == token {
			count++
		}
	}
	return float64(count)
}

// appendUniqueStr appends s to slice only if not already present.
func appendUniqueStr(slice []string, s string) []string {
	for _, existing := range slice {
		if existing == s {
			return slice
		}
	}
	return append(slice, s)
}
