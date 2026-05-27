package parser

import tree_sitter "github.com/tree-sitter/go-tree-sitter"

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
