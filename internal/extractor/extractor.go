// Package extractor implements IR extraction for all supported languages.
//
// Architecture position:
//
//	Parser → [Extractor] → Graph Builder
//
// The extractor converts a Tree-sitter syntax tree into a FileIR.
// Each language has its own extractor implementation. This file defines
// the shared Extractor interface and the router that selects the right
// extractor based on the parsed file's language.
//
// Adding a new language:
//  1. Create extractor_<lang>.go implementing Extractor
//  2. Register it in NewExtractorRouter()
//  3. Nothing else changes
package extractor

import (
	"fmt"

	"github.com/mohamamd-y-abbass/mearch/internal/ir"
	"github.com/mohamamd-y-abbass/mearch/internal/parser"
)

// Extractor extracts a FileIR from a parsed source file.
// Each language implements this interface independently.
type Extractor interface {
	// Extract produces a FileIR from a ParseResult.
	// Never returns nil FileIR — partial extraction is preferred over failure.
	Extract(result *parser.ParseResult) (*ir.FileIR, error)
}

// ExtractorRouter selects the correct Extractor for a given language.
// It is the only component that knows which extractor handles which language.
type ExtractorRouter struct {
	extractors map[parser.Language]Extractor
}

// NewExtractorRouter constructs an ExtractorRouter with all supported languages registered.
func NewExtractorRouter() *ExtractorRouter {
	r := &ExtractorRouter{
		extractors: make(map[parser.Language]Extractor),
	}

	// Register all language extractors.
	// Each extractor is stateless and safe for concurrent use.
	r.extractors[parser.LanguageGo] = NewGoExtractor()
	r.extractors[parser.LanguageTypeScript] = NewTypeScriptExtractor()
	r.extractors[parser.LanguageTSX] = NewTypeScriptExtractor() // TSX uses same extractor
	r.extractors[parser.LanguageJavaScript] = NewJavaScriptExtractor()
	r.extractors[parser.LanguagePython] = NewPythonExtractor()
	r.extractors[parser.LanguageRust] = NewRustExtractor()
	r.extractors[parser.LanguageJava] = NewJavaExtractor()

	return r
}

// Extract routes a ParseResult to the correct language extractor
// and returns the resulting FileIR.
func (r *ExtractorRouter) Extract(result *parser.ParseResult) (*ir.FileIR, error) {
	ext, ok := r.extractors[result.Language]
	if !ok {
		return nil, fmt.Errorf("extractor: no extractor registered for language %q", result.Language)
	}
	return ext.Extract(result)
}

// Supports reports whether the router has an extractor for the given language.
func (r *ExtractorRouter) Supports(lang parser.Language) bool {
	_, ok := r.extractors[lang]
	return ok
}
