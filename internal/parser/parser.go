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
// This package currently supports Go only.
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
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
)

// Language represents a supported source language.
//
// Using a typed constant (not a raw string) means:
//   - typos are caught at compile time
//   - switch statements can be exhaustive
//   - the zero value (LanguageUnknown) is safe and meaningful
type Language uint8

const (
	// LanguageUnknown is the zero value — returned when the file extension
	// has no registered grammar. Treat as "do not parse".
	LanguageUnknown Language = iota

	// LanguageGo represents Go source files (.go).
	LanguageGo

	// Future languages — uncomment as grammars are integrated.
	// Each requires a grammar import and a case in treeSitterLanguage().
	// LanguageTypeScript
	// LanguageJavaScript
	// LanguageTSX
	// LanguageJSX
	// LanguageRust
)

// ParseResult holds the output of a single parse operation.
//
// Ownership: ParseResult owns the Tree. Call result.Close() when done
// to release Tree-sitter C memory. The canonical pattern is:
//
//	result, err := p.ParseFile(ctx, path)
//	if err != nil { ... }
//	defer result.Close()
//
// Do NOT share a ParseResult across goroutines — the underlying Tree-sitter
// tree is not goroutine-safe.
type ParseResult struct {
	// Path is the absolute path of the parsed file.
	// Set even when parsing from bytes (ParseBytes) — used for error messages
	// and IR attribution.
	Path string

	// Language is the detected language of this file.
	Language Language

	// Source is the raw file content as parsed.
	//
	// IMPORTANT: Tree-sitter node positions (StartByte, EndByte) are byte
	// offsets into this exact slice. Do not modify Source after parsing —
	// byte offsets will become invalid. The IR extractor reads both the tree
	// and Source together.
	Source []byte

	// Tree is the Tree-sitter syntax tree produced by parsing Source.
	//
	// Tree-sitter is an error-recovering parser — it always produces a tree,
	// even for syntactically broken files. Check HasErrors() to know whether
	// the file parsed cleanly. Partial trees are still useful for indexing.
	//
	// Call Close() to free C memory when done.
	Tree *tree_sitter.Tree
}


// RootNode returns the root node of the syntax tree.
//
// This is the entry point for all IR extraction — the extractor receives
// the root node and Source together and walks from here.
//
// Returns nil if the ParseResult or Tree is nil (e.g. after Close()).
func (r *ParseResult) RootNode() *tree_sitter.Node {
	if r == nil || r.Tree == nil {
		return nil
	}
	return r.Tree.RootNode()
}

// HasErrors reports whether the syntax tree contains any parse errors.
//
// Tree-sitter recovers from syntax errors by inserting ERROR nodes into
// the tree rather than failing. A file with HasErrors() == true can still
// be partially indexed — the IR extractor skips nodes it cannot interpret.
//
// Prefer partial indexing over refusing to index. A broken file is still
// better indexed than not indexed at all.
func (r *ParseResult) HasErrors() bool {
	if r == nil || r.Tree == nil {
		return false
	}
	return r.Tree.RootNode().HasError()
}

// NodeContent extracts the source text for a given node using its byte offsets.
//
// Used heavily in the IR extractor — every symbol name, import path, and
// function signature is pulled out this way:
//
//	name := result.NodeContent(node)
func (r *ParseResult) NodeContent(node *tree_sitter.Node) string {
	if node == nil {
		return ""
	}
	start := node.StartByte()
	end := node.EndByte()
	if end > uint(len(r.Source)) {
		return ""
	}
	return string(r.Source[start:end])
}

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
		// tree_sitter.NewLanguage wraps the raw C language pointer returned
		// by the grammar binding into the Go-safe Language type.
		return tree_sitter.NewLanguage(tree_sitter_go.Language()), nil

	// Future grammars:
	// case LanguageTypeScript:
	// 	return tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTypescript()), nil
	// case LanguageTSX:
	// 	return tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTSX()), nil
	// case LanguageJavaScript:
	// 	return tree_sitter.NewLanguage(tree_sitter_javascript.Language()), nil

	default:
		return nil, fmt.Errorf("parser: no tree-sitter grammar registered for %q", lang)
	}
}

// --- Error types ---

// ErrUnsupportedLanguage is returned when a file's extension has no
// corresponding Tree-sitter grammar registered in this package.
type ErrUnsupportedLanguage struct {
	Path string
	Ext  string
}

func (e *ErrUnsupportedLanguage) Error() string {
	if e.Ext != "" {
		return fmt.Sprintf("parser: unsupported language (ext=%q, path=%s)", e.Ext, e.Path)
	}
	return fmt.Sprintf("parser: unsupported language (path=%s)", e.Path)
}
