// Package retrieval implements the retrieval engine for Mearch.
//
// The retrieval engine converts a natural language query into a ranked,
// compressed set of graph nodes representing the most relevant context
// for an AI coding agent.
//
// Pipeline (one file per stage):
//
//	preprocessor.go  — tokenize, stem, normalize
//	documents.go     — enrich symbols into searchable documents
//	bm25.go          — field-weighted BM25 index
//	expander.go      — query expansion
//	seeds.go         — multi-strategy seed detection
//	traversal.go     — bidirectional weighted BFS
//	scorer.go        — graph signal scoring
//	fusion.go        — score fusion + co-occurrence boost
//	compression.go   — tiered compression + file collapse
//	engine.go        — single Query() entry point
//
// No AI. No models. No external dependencies. No network calls.
// Everything computed locally from the codebase at index time.
package retrieval

import (
	"strings"
)

// stopWords are tokens that carry no retrieval signal and are dropped
// during preprocessing. Two categories:
//
//  1. English filler words ("the", "a", "is")
//  2. Generic programming action verbs that appear in nearly every
//     query and match nearly every symbol ("fix", "get", "update")
//
// These are stemmed forms — match after stemming is applied.
var stopWords = map[string]bool{
	// English filler
	"the": true, "a": true, "an": true, "is": true, "in": true,
	"at": true, "to": true, "of": true, "for": true, "on": true,
	"with": true, "this": true, "that": true, "it": true, "be": true,
	"as": true, "by": true, "or": true, "and": true, "not": true,
	"from": true, "its": true, "are": true, "was": true, "has": true,
	"have": true, "had": true, "but": true, "if": true, "do": true,
	"all": true, "can": true, "so": true, "we": true, "my": true,

	// Generic programming action verbs — too broad to be useful seeds.
	// "scanner" is meaningful; "fix" is not.
	"fix": true, "make": true, "get": true, "set": true, "add": true,
	"use": true, "run": true, "call": true, "help": true, "show": true,
	"need": true, "want": true, "will": true, "should": true,
	"update": true, "change": true, "creat": true, "delet": true,
	"remov": true, "check": true, "handl": true, "return": true,
	"work": true, "look": true, "find": true, "let": true, "new": true,
}

// Preprocessor normalizes queries and symbol names into comparable
// token sets. Stateless — safe for concurrent use.
type Preprocessor struct{}

// NewPreprocessor constructs a Preprocessor.
func NewPreprocessor() *Preprocessor {
	return &Preprocessor{}
}

// ProcessQuery tokenizes, normalizes, and stems a natural language query.
//
// Pipeline:
//  1. Split on whitespace + punctuation
//  2. Lowercase
//  3. camelCase split (agents sometimes paste symbol names into queries)
//  4. Remove stop words
//  5. Stem
//  6. Deduplicate
//
// Returns stemmed tokens ready for seed detection and BM25 query vector.
//
// Example:
//
//	"fix the file discovery ignore rules"
//	→ ["file", "discov", "ignor", "rule"]
func (p *Preprocessor) ProcessQuery(query string) []string {
	raw := p.splitRaw(query)
	seen := make(map[string]bool)
	result := make([]string, 0, len(raw))

	for _, tok := range raw {
		// camelCase split — agents paste symbol names into queries.
		words := splitIdentifier(tok)
		for _, w := range words {
			lower := strings.ToLower(w)
			if len(lower) < 2 {
				continue
			}
			if stopWords[lower] {
				continue
			}
			s := stem(lower)
			if len(s) < 2 || stopWords[s] {
				continue
			}
			if !seen[s] {
				seen[s] = true
				result = append(result, s)
			}
		}
	}

	return result
}

// ProcessSymbol tokenizes and stems a symbol name for indexing.
//
// Handles camelCase, PascalCase, snake_case, qualified names.
//
// Examples:
//
//	"ShouldIgnore"                        → ["should", "ignor"]
//	"scanner.Scanner.Scan"                → ["scanner", "scan"]
//	"filepath.WalkDir"                    → ["filepath", "walk", "dir"]
//	"MAX_RETRIES"                         → ["max", "retri"]
func (p *Preprocessor) ProcessSymbol(name string) []string {
	// Treat dots as separators for qualified names.
	name = strings.ReplaceAll(name, ".", " ")
	name = strings.ReplaceAll(name, "_", " ")

	raw := p.splitRaw(name)
	seen := make(map[string]bool)
	result := make([]string, 0, len(raw)*2)

	for _, tok := range raw {
		words := splitIdentifier(tok)
		for _, w := range words {
			lower := strings.ToLower(w)
			if len(lower) < 2 {
				continue
			}
			s := stem(lower)
			if len(s) < 2 {
				continue
			}
			if !seen[s] {
				seen[s] = true
				result = append(result, s)
			}
		}
	}

	return result
}

// ProcessText tokenizes and stems arbitrary text (comments, docstrings).
//
// Same as ProcessQuery but does not apply stop words — in documents
// we want to preserve more terms for BM25 matching. Stop words are
// applied during query processing, not document processing, so that
// the IDF calculation reflects the true corpus distribution.
func (p *Preprocessor) ProcessText(text string) []string {
	// Strip comment markers.
	text = strings.TrimPrefix(text, "//")
	text = strings.TrimPrefix(text, "/*")
	text = strings.TrimSuffix(text, "*/")
	text = strings.TrimSpace(text)

	raw := p.splitRaw(text)
	seen := make(map[string]bool)
	result := make([]string, 0, len(raw))

	for _, tok := range raw {
		words := splitIdentifier(tok)
		for _, w := range words {
			lower := strings.ToLower(w)
			if len(lower) < 2 {
				continue
			}
			s := stem(lower)
			if len(s) < 2 {
				continue
			}
			if !seen[s] {
				seen[s] = true
				result = append(result, s)
			}
		}
	}

	return result
}

// ProcessPath tokenizes a file path into searchable tokens.
//
// Example:
//
//	"/project/internal/scanner/scanner.go"
//	→ ["project", "intern", "scanner"]
func (p *Preprocessor) ProcessPath(path string) []string {
	// Normalize separators.
	path = strings.ReplaceAll(path, "/", " ")
	path = strings.ReplaceAll(path, "\\", " ")
	path = strings.ReplaceAll(path, ".", " ")
	path = strings.ReplaceAll(path, "_", " ")
	path = strings.ReplaceAll(path, "-", " ")

	raw := p.splitRaw(path)
	seen := make(map[string]bool)
	result := make([]string, 0, len(raw))

	for _, tok := range raw {
		lower := strings.ToLower(tok)
		if len(lower) < 2 {
			continue
		}
		s := stem(lower)
		if len(s) < 2 {
			continue
		}
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}

	return result
}

// Tokens returns the raw unstemmed lowercase tokens from a string.
// Used when we need the original words for display or synonym lookup.
func (p *Preprocessor) Tokens(s string) []string {
	raw := p.splitRaw(s)
	result := make([]string, 0, len(raw))
	seen := make(map[string]bool)
	for _, tok := range raw {
		lower := strings.ToLower(tok)
		if len(lower) >= 2 && !seen[lower] && isAlphanumeric(lower) {
			seen[lower] = true
			result = append(result, lower)
		}
	}
	return result
}

// splitRaw splits a string into lowercase alphanumeric tokens on all
// non-letter, non-digit boundaries.
func (p *Preprocessor) splitRaw(s string) []string {
	// Replace common separators with spaces.
	replacer := strings.NewReplacer(
		"_", " ", "-", " ", ".", " ", "/", " ", "\\", " ",
		"(", " ", ")", " ", "{", " ", "}", " ",
		"[", " ", "]", " ", ",", " ", ";", " ",
		":", " ", "*", " ", "&", " ", "<", " ", ">", " ",
		"\"", " ", "'", " ", "`", " ",
	)
	s = replacer.Replace(s)

	fields := strings.Fields(s)
	result := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) >= 2 && isAlphanumeric(f) {
			result = append(result, f)
		}
	}
	return result
}
