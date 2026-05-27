// Package parser implements the syntax parsing layer for Mearch.
//
// The parser sits directly after the scanner in the pipeline:
//
//	Scanner → [Parser] → IR Extractor → Graph Builder → Retrieval
//
// Responsibility: read source file bytes and produce a syntax tree using
// Tree-sitter. The parser does NOT extract semantic meaning — that is the
// IR extractor's job. The parser's only output is a ParseResult containing
// the raw source bytes and the Tree-sitter syntax tree.
//
// Supported languages: Go, Python, JavaScript, TypeScript, TSX, Rust, C,
// C++, Java, Bash, JSON, HTML, CSS.
// Adding a new language requires three steps:
//  1. Add a Language constant below
//  2. Add the grammar import and a case in treeSitterLanguage()
//  3. Add the extension mapping in languageForExt
//
// Nothing else in the engine needs to change.
//
// Setup — add these to go.mod:
//
//	go get github.com/tree-sitter/go-tree-sitter@latest
//	go get github.com/tree-sitter/tree-sitter-go/bindings/go@latest
package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_bash "github.com/tree-sitter/tree-sitter-bash/bindings/go"
	tree_sitter_c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tree_sitter_cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tree_sitter_css "github.com/tree-sitter/tree-sitter-css/bindings/go"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tree_sitter_html "github.com/tree-sitter/tree-sitter-html/bindings/go"
	tree_sitter_java "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tree_sitter_javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tree_sitter_json "github.com/tree-sitter/tree-sitter-json/bindings/go"
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tree_sitter_rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// Parser parses source files into Tree-sitter syntax trees.
//
// # Concurrency
//
// Parser is NOT safe for concurrent use. The underlying tree_sitter.Parser
// cannot be shared across goroutines. For concurrent indexing, create one
// Parser per worker goroutine:
//
//	// Worker goroutine pattern:
//	p := parser.NewParser()
//	defer p.Close()
//	for path := range fileCh {
//	    result, err := p.ParseFile(ctx, path)
//	    ...
//	}
type Parser struct {
	// inner is the Tree-sitter parser instance.
	// Not goroutine-safe — one Parser per goroutine for concurrent use.
	inner *tree_sitter.Parser
}

// NewParser constructs a new Parser.
//
// Always pair with defer p.Close() to release Tree-sitter C resources:
//
//	p := parser.NewParser()
//	defer p.Close()
func NewParser() *Parser {
	return &Parser{
		inner: tree_sitter.NewParser(),
	}
}

// Close releases the Tree-sitter C memory held by this Parser.
// Safe to call on a nil Parser or after already closing.
func (p *Parser) Close() {
	if p != nil && p.inner != nil {
		p.inner.Close()
		p.inner = nil
	}
}

// ParseFile reads the file at path from disk and parses it into a syntax tree.
//
// The language is detected automatically from the file extension.
// Returns ErrUnsupportedLanguage if the extension has no registered grammar.
//
// The caller must call result.Close() to free Tree-sitter memory.
//
// Context is checked before I/O begins. If cancelled, returns immediately
// with ctx.Err(). Note: Tree-sitter itself does not support mid-parse
// cancellation — cancellation only applies before parsing starts.
func (p *Parser) ParseFile(ctx context.Context, path string) (*ParseResult, error) {
	// Check cancellation before any I/O.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	// Detect language before touching the filesystem.
	// Failing fast here avoids a pointless file read for unsupported types.
	lang := LanguageForFile(path)
	if lang == LanguageUnknown {
		return nil, &ErrUnsupportedLanguage{
			Path: path,
			Ext:  filepath.Ext(path),
		}
	}

	// Read the full file content.
	// os.ReadFile uses the file size as a hint for the initial allocation,
	// making it more efficient than a generic bufio reader for this use case.
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("parser: read %s: %w", path, err)
	}

	return p.ParseBytes(ctx, path, lang, src)
}

// ParseBytes parses a pre-loaded source byte slice without touching the
// filesystem.
//
// Use this when:
//   - File content is already in memory (editor buffer, test fixture)
//   - Parsing modified content before it has been written to disk
//   - Writing unit tests without real files
//
// path is used only for ParseResult.Path and error messages — it does not
// need to refer to a real file.
// lang must not be LanguageUnknown.
func (p *Parser) ParseBytes(ctx context.Context, path string, lang Language, src []byte) (*ParseResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if lang == LanguageUnknown {
		return nil, &ErrUnsupportedLanguage{Path: path}
	}

	// Resolve the Tree-sitter grammar for this language.
	tsLang, err := treeSitterLanguage(lang)
	if err != nil {
		return nil, err
	}

	// Set the grammar on the parser.
	// Safe to call between parses — Tree-sitter handles grammar switching cleanly.
	p.inner.SetLanguage(tsLang)

	// Parse the source bytes.
	//
	// The second argument (nil) means "no previous tree" — we always do full
	// parses here. Incremental reparsing (passing the old tree after an edit)
	// will be wired in when the watcher layer is built in Phase 3.
	//
	// Parse() always returns a tree due to error recovery. A nil return
	// means an internal Tree-sitter failure, not a syntax error in the source.
	tree := p.inner.Parse(src, nil)
	if tree == nil {
		return nil, fmt.Errorf("parser: tree-sitter returned nil tree for %s (internal error)", path)
	}

	return &ParseResult{
		Path:     path,
		Language: lang,
		Source:   src,
		Tree:     tree,
	}, nil
}

// treeSitterLanguage maps a Language constant to its Tree-sitter grammar.
//
// This is the only location that imports language-specific grammar packages.
// Adding a language = adding one import + one case here. Nothing else changes.
func treeSitterLanguage(lang Language) (*tree_sitter.Language, error) {
	switch lang {
	case LanguageGo:
		return tree_sitter.NewLanguage(tree_sitter_go.Language()), nil
	case LanguagePython:
		return tree_sitter.NewLanguage(tree_sitter_python.Language()), nil
	case LanguageJavaScript:
		return tree_sitter.NewLanguage(tree_sitter_javascript.Language()), nil
	case LanguageTypeScript:
		return tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTypescript()), nil
	case LanguageTSX:
		return tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTSX()), nil
	case LanguageRust:
		return tree_sitter.NewLanguage(tree_sitter_rust.Language()), nil
	case LanguageC:
		return tree_sitter.NewLanguage(tree_sitter_c.Language()), nil
	case LanguageCPP:
		return tree_sitter.NewLanguage(tree_sitter_cpp.Language()), nil
	case LanguageJava:
		return tree_sitter.NewLanguage(tree_sitter_java.Language()), nil
	case LanguageBash:
		return tree_sitter.NewLanguage(tree_sitter_bash.Language()), nil
	case LanguageJSON:
		return tree_sitter.NewLanguage(tree_sitter_json.Language()), nil
	case LanguageHTML:
		return tree_sitter.NewLanguage(tree_sitter_html.Language()), nil
	case LanguageCSS:
		return tree_sitter.NewLanguage(tree_sitter_css.Language()), nil
	default:
		return nil, fmt.Errorf("parser: no tree-sitter grammar registered for %q", lang)
	}
}
