package retrieval

import (
	"math"
	"strings"
	"unicode"
)

func vectorMagnitude(v map[string]float64) float64 {
	sum := 0.0
	for _, val := range v {
		sum += val * val
	}
	return math.Sqrt(sum)
}

// appendUnique appends id to slice only if not already present.
func appendUnique(slice []string, id string) []string {
	for _, existing := range slice {
		if existing == id {
			return slice
		}
	}
	return append(slice, id)
}

// containsToken reports whether token appears in tokens.
func containsToken(tokens []string, token string) bool {
	for _, t := range tokens {
		if t == token {
			return true
		}
	}
	return false
}

// tokenOverlap returns the number of tokens shared between two slices.
func tokenOverlap(a, b []string) int {
	set := make(map[string]bool, len(a))
	for _, t := range a {
		set[t] = true
	}
	count := 0
	for _, t := range b {
		if set[t] {
			count++
		}
	}
	return count
}

// clamp clamps v to [lo, hi].
func clamp(v, lo, hi float64) float64 {
	return maxFloat(lo, minFloat(hi, v))
}

// maxFloat returns the larger of two float64 values.
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// minFloat returns the smaller of two float64 values.
func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// min3Int returns the minimum of three integers.
func min3Int(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// LevenshteinDistance computes the minimum edit distance between a and b.
//
// Used in seed detection for fuzzy symbol name matching.
// Allows "scaner" to match "scanner" (distance 1).
//
// Implementation uses two-row DP for O(min(|a|,|b|)) space.
func LevenshteinDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)

	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	// Ensure a is the shorter string for space efficiency.
	if la > lb {
		ra, rb = rb, ra
		la, lb = lb, la
	}

	prev := make([]int, la+1)
	curr := make([]int, la+1)

	for i := range prev {
		prev[i] = i
	}

	for j := 1; j <= lb; j++ {
		curr[0] = j
		for i := 1; i <= la; i++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[i] = min3Int(curr[i-1]+1, prev[i]+1, prev[i-1]+cost)
		}
		prev, curr = curr, prev
	}

	return prev[la]
}

// isAlphanumeric reports whether s contains only letters and digits.
func isAlphanumeric(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// --- Stemmer ---
//
// Lightweight suffix-stripping stemmer covering common English suffixes
// that appear in programming vocabulary. Not a full Porter stemmer —
// covers the 80% of cases that matter for code search.
//
// Design principle: consistent normalization matters more than
// linguistic accuracy. "scanner" and "scanning" both becoming "scan"
// is what we need. Perfect morphological analysis is not.
// stem reduces a word to its approximate root form.
//
// Examples:
//
//	"scanning"    → "scan"
//	"discovery"   → "discov"
//	"ignoring"    → "ignor"
//	"directories" → "director"
//	"files"       → "file"
//	"imported"    → "import"
//	"retrieval"   → "retriev"
func stem(word string) string {
	if len(word) <= 3 {
		return word
	}

	// Rules ordered from most specific (longest suffix) to least specific.
	// Each rule only fires if the remaining stem is long enough to be meaningful.

	type rule struct {
		suffix  string
		replace string
		minLen  int // minimum length of word to apply this rule
	}

	rules := []rule{
		// 6-char suffixes
		{"nesses", "", 8},
		{"lessly", "", 8},
		{"ations", "", 8},

		// 5-char suffixes
		{"iness", "", 7},
		{"ation", "", 7},
		{"alism", "", 7},
		{"alist", "", 7},
		{"ality", "", 7},
		{"alize", "", 7},
		{"ement", "", 7},

		// 4-char suffixes
		{"ness", "", 6},
		{"ment", "", 6},
		{"less", "", 6},
		{"tion", "", 6},
		{"sion", "", 6},
		{"ries", "ry", 5},
		{"ying", "", 5},
		{"ting", "", 5},
		{"ring", "", 5},
		{"ling", "", 5},
		{"king", "", 5},
		{"ning", "", 5},
		{"sing", "", 5},
		{"ding", "", 5},

		// 3-char suffixes
		{"ing", "", 5},
		{"ery", "", 5},
		{"ary", "", 5},
		{"ory", "", 5},
		{"ity", "", 5},
		{"ive", "", 5},
		{"ize", "", 5},
		{"ise", "", 5},
		{"ies", "y", 4},
		{"ers", "", 5},
		{"ors", "", 5},
		{"als", "", 5},
		{"ful", "", 5},
		{"ous", "", 5},

		// 2-char suffixes
		{"er", "", 4},
		{"or", "", 4},
		{"ed", "", 4},
		{"ly", "", 4},
		{"al", "", 4},
		{"ic", "", 4},

		// Plural -s (careful — don't strip from "class", "process", "access")
		{"s", "", 4},
	}

	for _, r := range rules {
		if len(word) >= r.minLen && strings.HasSuffix(word, r.suffix) {
			stem := word[:len(word)-len(r.suffix)] + r.replace
			// Sanity check — stem must be at least 2 chars.
			if len(stem) >= 2 {
				return stem
			}
		}
	}

	return word
}

// --- camelCase splitter ---

// splitIdentifier splits a camelCase or PascalCase identifier into
// constituent words.
//
// Examples:
//
//	"ShouldIgnore"  → ["Should", "Ignore"]
//	"NewScanner"    → ["New", "Scanner"]
//	"parseHTML"     → ["parse", "HTML"]
//	"WalkDir"       → ["Walk", "Dir"]
//	"filepath"      → ["filepath"]
func splitIdentifier(s string) []string {
	if s == "" {
		return nil
	}

	var words []string
	var current strings.Builder
	runes := []rune(s)

	for i, r := range runes {
		if i == 0 {
			current.WriteRune(r)
			continue
		}

		prev := runes[i-1]
		var next rune
		if i+1 < len(runes) {
			next = runes[i+1]
		}

		shouldSplit := false

		if unicode.IsUpper(r) {
			if unicode.IsLower(prev) {
				// "parseHtml" → split before H
				shouldSplit = true
			} else if unicode.IsUpper(prev) && next != 0 && unicode.IsLower(next) {
				// "HTMLParser" → split before P
				shouldSplit = true
			}
		} else if unicode.IsDigit(r) && unicode.IsLetter(prev) {
			// "field1" → split before 1
			shouldSplit = true
		} else if unicode.IsLetter(r) && unicode.IsDigit(prev) {
			// "1field" → split before f
			shouldSplit = true
		}

		if shouldSplit && current.Len() > 0 {
			words = append(words, current.String())
			current.Reset()
		}

		current.WriteRune(r)
	}

	if current.Len() > 0 {
		words = append(words, current.String())
	}

	return words
}
