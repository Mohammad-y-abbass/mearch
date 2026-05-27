package retrieval

import (
	"strings"

	"github.com/mohamamd-y-abbass/mearch/internal/graph"
	"github.com/mohamamd-y-abbass/mearch/internal/ir"
)

// Document is an enriched, searchable representation of a graph node.
//
// Each node in the graph becomes one Document. The document contains
// far more than just the node's name — it includes all the contextual
// information that makes BM25 search useful:
// comments, callers, callees, siblings, file path, package.
//
// The richer the document, the better BM25 performs.
// A query for "walk directory files" should find scanner.Scanner.Scan
// through its comment — not just through its name.
//
// Documents are built once at index time and stored alongside the graph.
type Document struct {
	// NodeID is the graph node this document represents.
	NodeID string

	// Fields are the individual searchable text fields.
	// BM25 is applied per-field with different weights.
	Fields map[FieldName][]string // field → stemmed tokens

	// RawFields stores the original unstemmed text per field.
	// Used for display in ContextResult output.
	RawFields map[FieldName]string

	// TotalTokens is the sum of tokens across all fields.
	// Used for BM25 length normalization.
	TotalTokens int
}

// FieldName identifies a document field.
// BM25 applies different weights to different fields — a query word
// in the symbol name is a stronger signal than the same word in an import.
type FieldName string

const (
	// FieldName — ordered from highest to lowest BM25 weight.
	FieldName_Name       FieldName = "name"       // weight 3.0 — symbol name
	FieldName_Qualified  FieldName = "qualified"  // weight 2.5 — full qualified name
	FieldName_Comment    FieldName = "comment"    // weight 2.0 — docstring / comment
	FieldName_Callers    FieldName = "callers"    // weight 1.5 — who calls this
	FieldName_Callees    FieldName = "callees"    // weight 1.5 — what this calls
	FieldName_Siblings   FieldName = "siblings"   // weight 1.2 — other methods on same receiver
	FieldName_Package    FieldName = "package"    // weight 1.0 — package name
	FieldName_FilePath   FieldName = "filepath"   // weight 1.0 — file path
	FieldName_Imports    FieldName = "imports"    // weight 0.5 — imported packages
	FieldName_ReturnType FieldName = "returntype" // weight 0.8 — return type names
	FieldName_ParamType  FieldName = "paramtype"  // weight 0.8 — parameter type names
)

// fieldWeights maps field names to their BM25 weight multiplier.
// Higher weight = stronger signal for ranking.
// Tuned for code search: name and comment are highest value.
var fieldWeights = map[FieldName]float64{
	FieldName_Name:       3.0,
	FieldName_Qualified:  2.5,
	FieldName_Comment:    2.0,
	FieldName_Callers:    1.5,
	FieldName_Callees:    1.5,
	FieldName_Siblings:   1.2,
	FieldName_ReturnType: 0.8,
	FieldName_ParamType:  0.8,
	FieldName_Package:    1.0,
	FieldName_FilePath:   1.0,
	FieldName_Imports:    0.5,
}

// DocumentBuilder builds enriched Documents from the graph and FileIRs.
type DocumentBuilder struct {
	prep *Preprocessor
}

// NewDocumentBuilder constructs a DocumentBuilder.
func NewDocumentBuilder() *DocumentBuilder {
	return &DocumentBuilder{prep: NewPreprocessor()}
}

// BuildAll constructs enriched Documents for every node in the graph.
//
// The graph provides structural information (edges, node kinds).
// The fileIRs provide semantic information (function signatures, imports).
// Together they produce documents rich enough for high-quality BM25 search.
//
// Returns a map of nodeID → Document for O(1) lookup during BM25 scoring.
func (b *DocumentBuilder) BuildAll(g *graph.Graph, fileIRs []*ir.FileIR) map[string]*Document {
	docs := make(map[string]*Document, len(g.AllNodes()))

	// Build auxiliary lookups from fileIRs before iterating nodes.
	// These are expensive to compute per-node but cheap to build once.
	callerMap := b.buildCallerMap(g)         // nodeID → []callerID
	calleeMap := b.buildCalleeMap(g)         // nodeID → []calleeID
	siblingMap := b.buildSiblingMap(fileIRs) // qualifiedName → []siblingName
	commentMap := b.buildCommentMap(fileIRs) // qualifiedName → comment text
	importMap := b.buildImportMap(fileIRs)   // filePath → []importPath

	for _, node := range g.AllNodes() {
		doc := b.buildDocument(node, g, callerMap, calleeMap, siblingMap, commentMap, importMap)
		docs[node.ID] = doc
	}

	return docs
}

// buildDocument constructs a single enriched Document for a graph node.
func (b *DocumentBuilder) buildDocument(
	node *graph.Node,
	g *graph.Graph,
	callerMap map[string][]string,
	calleeMap map[string][]string,
	siblingMap map[string][]string,
	commentMap map[string]string,
	importMap map[string][]string,
) *Document {
	doc := &Document{
		NodeID:    node.ID,
		Fields:    make(map[FieldName][]string),
		RawFields: make(map[FieldName]string),
	}

	// --- Field: name ---
	// The symbol's short display name. Highest weight field.
	// camelCase split so "ShouldIgnore" → ["should", "ignor"].
	b.setField(doc, FieldName_Name, node.Name)

	// --- Field: qualified ---
	// Full qualified name including package and receiver.
	// "scanner.Scanner.Scan" → ["scanner", "scan"]
	b.setField(doc, FieldName_Qualified, node.ID)

	// --- Field: package ---
	b.setField(doc, FieldName_Package, node.Package)

	// --- Field: filepath ---
	if node.FilePath != "" {
		tokens := b.prep.ProcessPath(node.FilePath)
		doc.Fields[FieldName_FilePath] = tokens
		doc.RawFields[FieldName_FilePath] = node.FilePath
	}

	// --- Field: comment ---
	// Docstrings written in human language are the most valuable field
	// for closing the vocabulary gap between queries and symbol names.
	if comment, ok := commentMap[node.ID]; ok && comment != "" {
		tokens := b.prep.ProcessText(comment)
		doc.Fields[FieldName_Comment] = tokens
		doc.RawFields[FieldName_Comment] = comment
	}

	// --- Field: callers ---
	// Who calls this node? Names of callers expand the document with
	// the vocabulary of the context where this symbol is used.
	if callers, ok := callerMap[node.ID]; ok {
		var callerTokens []string
		var callerNames []string
		for _, callerID := range callers {
			callerNode := g.Node(callerID)
			if callerNode == nil {
				continue
			}
			callerNames = append(callerNames, callerNode.Name)
			callerTokens = append(callerTokens, b.prep.ProcessSymbol(callerNode.Name)...)
		}
		if len(callerTokens) > 0 {
			doc.Fields[FieldName_Callers] = dedupTokens(callerTokens)
			doc.RawFields[FieldName_Callers] = strings.Join(callerNames, " ")
		}
	}

	// --- Field: callees ---
	// What does this node call? Expands the document with the vocabulary
	// of what this symbol depends on.
	if callees, ok := calleeMap[node.ID]; ok {
		var calleeTokens []string
		var calleeNames []string
		for _, calleeID := range callees {
			calleeNode := g.Node(calleeID)
			if calleeNode == nil {
				continue
			}
			calleeNames = append(calleeNames, calleeNode.Name)
			calleeTokens = append(calleeTokens, b.prep.ProcessSymbol(calleeNode.Name)...)
		}
		if len(calleeTokens) > 0 {
			doc.Fields[FieldName_Callees] = dedupTokens(calleeTokens)
			doc.RawFields[FieldName_Callees] = strings.Join(calleeNames, " ")
		}
	}

	// --- Field: siblings ---
	// Other methods on the same receiver type / functions in the same file.
	// If Scanner has Scan, IsSupported, and RootDir — searching for any of
	// those terms can surface Scanner and all its siblings.
	if siblings, ok := siblingMap[node.ID]; ok {
		var siblingTokens []string
		for _, sib := range siblings {
			siblingTokens = append(siblingTokens, b.prep.ProcessSymbol(sib)...)
		}
		if len(siblingTokens) > 0 {
			doc.Fields[FieldName_Siblings] = dedupTokens(siblingTokens)
			doc.RawFields[FieldName_Siblings] = strings.Join(siblings, " ")
		}
	}

	// --- Field: imports ---
	// Import paths for file nodes. Lowest weight — "os", "fmt" appear
	// in almost every file and carry little discriminating signal.
	if node.Kind == graph.NodeKindFile {
		if imports, ok := importMap[node.ID]; ok {
			var importTokens []string
			for _, imp := range imports {
				// Only the last path segment is useful: "path/filepath" → "filepath"
				parts := strings.Split(imp, "/")
				last := parts[len(parts)-1]
				importTokens = append(importTokens, b.prep.ProcessSymbol(last)...)
			}
			if len(importTokens) > 0 {
				doc.Fields[FieldName_Imports] = dedupTokens(importTokens)
				doc.RawFields[FieldName_Imports] = strings.Join(imports, " ")
			}
		}
	}

	// Compute total token count for BM25 length normalization.
	total := 0
	for _, tokens := range doc.Fields {
		total += len(tokens)
	}
	doc.TotalTokens = total

	return doc
}

// setField tokenizes a symbol name and stores it in the document field.
func (b *DocumentBuilder) setField(doc *Document, field FieldName, text string) {
	if text == "" {
		return
	}
	tokens := b.prep.ProcessSymbol(text)
	if len(tokens) > 0 {
		doc.Fields[field] = tokens
		doc.RawFields[field] = text
	}
}

// --- Auxiliary map builders ---

// buildCallerMap builds nodeID → []callerNodeID from call edges.
func (b *DocumentBuilder) buildCallerMap(g *graph.Graph) map[string][]string {
	callers := make(map[string][]string)
	for _, node := range g.AllNodes() {
		for _, edge := range g.InEdges(node.ID) {
			if edge.Kind == graph.EdgeKindCall {
				callers[node.ID] = append(callers[node.ID], edge.From)
			}
		}
	}
	return callers
}

// buildCalleeMap builds nodeID → []calleeNodeID from call edges.
func (b *DocumentBuilder) buildCalleeMap(g *graph.Graph) map[string][]string {
	callees := make(map[string][]string)
	for _, node := range g.AllNodes() {
		for _, edge := range g.OutEdges(node.ID) {
			if edge.Kind == graph.EdgeKindCall {
				callees[node.ID] = append(callees[node.ID], edge.To)
			}
		}
	}
	return callees
}

// buildSiblingMap builds qualifiedName → []siblingNames from FileIRs.
//
// Siblings are other methods on the same receiver type, or other
// functions in the same file. This enriches struct/interface documents
// with the names of all their methods.
func (b *DocumentBuilder) buildSiblingMap(fileIRs []*ir.FileIR) map[string][]string {
	siblings := make(map[string][]string)

	for _, file := range fileIRs {
		// Group functions by receiver.
		byReceiver := make(map[string][]string)
		var plainFuncs []string

		for _, fn := range file.Functions {
			if fn.Receiver != "" {
				byReceiver[fn.Receiver] = append(byReceiver[fn.Receiver], fn.Name)
			} else {
				plainFuncs = append(plainFuncs, fn.Name)
			}
		}

		// For each method, its siblings are the other methods on the same receiver.
		for _, fn := range file.Functions {
			if fn.Receiver != "" {
				sibs := byReceiver[fn.Receiver]
				var filtered []string
				for _, s := range sibs {
					if s != fn.Name {
						filtered = append(filtered, s)
					}
				}
				if len(filtered) > 0 {
					siblings[fn.Qualified] = filtered
				}
			}
		}

		// For plain functions, siblings are other plain functions in the same file.
		for _, fn := range file.Functions {
			if fn.Receiver == "" && len(plainFuncs) > 1 {
				var filtered []string
				for _, s := range plainFuncs {
					if s != fn.Name {
						filtered = append(filtered, s)
					}
				}
				siblings[fn.Qualified] = filtered
			}
		}

		// For struct/interface nodes, siblings are all their methods.
		for _, sym := range file.Symbols {
			if methods, ok := byReceiver[sym.Name]; ok {
				siblings[sym.Qualified] = methods
			}
		}
	}

	return siblings
}

// buildCommentMap builds qualifiedName → comment text from FileIRs.
//
// NOTE: Currently synthesizes comments from qualified names since
// Tree-sitter comment extraction requires additional query work.
// When comment extraction is added to the IR extractor, this map
// will be populated with real docstrings.
func (b *DocumentBuilder) buildCommentMap(fileIRs []*ir.FileIR) map[string]string {
	comments := make(map[string]string)

	// For now, generate synthetic comments from the qualified name itself.
	// "scanner.Scanner.Scan" → "Scan walks the scanner Scanner"
	// This is intentionally thin until real comment extraction lands.
	// The significant vocabulary expansion comes from callers/callees/siblings.
	for _, file := range fileIRs {
		for _, fn := range file.Functions {
			// Synthesize: split the qualified name into human-readable form.
			parts := splitIdentifier(fn.Name)
			comments[fn.Qualified] = strings.Join(parts, " ")
		}
		for _, sym := range file.Symbols {
			parts := splitIdentifier(sym.Name)
			comments[sym.Qualified] = strings.Join(parts, " ")
		}
	}

	return comments
}

// buildImportMap builds filePath → []importPaths from FileIRs.
func (b *DocumentBuilder) buildImportMap(fileIRs []*ir.FileIR) map[string][]string {
	imports := make(map[string][]string)
	for _, file := range fileIRs {
		for _, imp := range file.Imports {
			if imp.Path != "" {
				imports[file.Path] = append(imports[file.Path], imp.Path)
			}
		}
	}
	return imports
}

// --- helpers ---

// dedupTokens removes duplicate tokens from a slice while preserving order.
func dedupTokens(tokens []string) []string {
	seen := make(map[string]bool, len(tokens))
	result := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if !seen[t] {
			seen[t] = true
			result = append(result, t)
		}
	}
	return result
}
