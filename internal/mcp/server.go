package mcp

// Package mcp implements the MCP server for Mearch.
//
// The MCP server exposes the retrieval engine as a set of tools that
// AI coding agents (Claude, Cursor, Windsurf, etc.) can call to get
// relevant code context without scanning entire repositories.
//
// Transport: stdio (standard input/output)
// Protocol:  Model Context Protocol 2025-11-25
//
// Tools exposed:
//
//	index_project    — scan and index a project directory
//	query_context    — retrieve relevant context for a natural language query
//	find_symbol      — find a specific symbol by name
//	find_callers     — find all callers of a symbol
//	trace_deps       — trace dependencies of a symbol
//	graph_stats      — return graph statistics for the indexed project
//
// Usage (add to your MCP client config):
//
//	{
//	  "mcpServers": {
//	    "mearch": {
//	      "command": "mearch",
//	      "args": ["serve"]
//	    }
//	  }
//	}

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mohamamd-y-abbass/mearch/internal/extractor"
	"github.com/mohamamd-y-abbass/mearch/internal/graph"
	"github.com/mohamamd-y-abbass/mearch/internal/ir"
	"github.com/mohamamd-y-abbass/mearch/internal/parser"
	"github.com/mohamamd-y-abbass/mearch/internal/retrieval"
	"github.com/mohamamd-y-abbass/mearch/internal/scanner"
)

// mearchServer holds the server state.
// The index is built lazily on the first index_project call and
// rebuilt whenever index_project is called again with a new path.
type mearchServer struct {
	mu      sync.RWMutex
	engine  *retrieval.Engine
	g       *graph.Graph
	rootDir string
}

func newMearchServer() *mearchServer {
	return &mearchServer{}
}

func New() *mearchServer {
	return newMearchServer()
}

// run creates the MCP server, registers all tools, and starts serving on stdio.
func (s *mearchServer) Run() error {
	// Create MCP server.
	mcpServer := sdk.NewServer(&sdk.Implementation{
		Name:    "mearch",
		Version: "0.1.0",
	}, nil)

	// Register tools.
	s.registerIndexProject(mcpServer)
	s.registerQueryContext(mcpServer)
	s.registerFindSymbol(mcpServer)
	s.registerFindCallers(mcpServer)
	s.registerTraceDeps(mcpServer)
	s.registerGraphStats(mcpServer)

	// Serve over stdio.
	ctx := context.Background()

	return mcpServer.Run(
		ctx,
		&sdk.StdioTransport{},
	)
}

// =========================================================
// Tool: index_project
// =========================================================

type indexProjectArgs struct {
	// Absolute or relative path to the project root to index.
	Path string `json:"path"`

	// Maximum directory depth to scan (0 = unlimited).
	MaxDepth int `json:"max_depth"`
}

func (s *mearchServer) registerIndexProject(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name: "index_project",
		Description: `Index a project directory for code intelligence retrieval.

Must be called before any other Mearch tools. Scans the project,
parses all source files, builds a semantic graph, and prepares the
retrieval index. Re-call to re-index after significant code changes.

Returns a summary of what was indexed including file count, graph size,
and any files that could not be parsed.`,
	}, func(ctx context.Context, req *sdk.CallToolRequest, args indexProjectArgs) (*sdk.CallToolResult, any, error) {
		result, err := s.indexProject(ctx, args)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return textResult(result), nil, nil
	})
}

func (s *mearchServer) indexProject(ctx context.Context, args indexProjectArgs) (string, error) {
	if args.Path == "" {
		return "", errors.New("path is required")
	}
	if args.MaxDepth == 0 {
		args.MaxDepth = 10
	}

	// --- Scan ---
	sc, err := scanner.NewScanner(args.Path, scanner.ScanOptions{
		MaxDepth: args.MaxDepth,
	})
	if err != nil {
		return "", fmt.Errorf("scanner init failed: %w", err)
	}

	files, scanErr := sc.Scan()
	var scanWarnings []string
	var scanErrs scanner.ScanErrors
	if errors.As(scanErr, &scanErrs) {
		for _, e := range scanErrs {
			scanWarnings = append(scanWarnings, e.Error())
		}
	} else if scanErr != nil {
		return "", fmt.Errorf("scan failed: %w", scanErr)
	}

	// --- Parse + Extract IR ---
	p := parser.NewParser()
	defer p.Close()

	ext := extractor.NewExtractorRouter()

	var (
		fileIRs      []*ir.FileIR
		parseErrors  []string
		skippedCount int
	)

	for _, path := range files {
		lang := parser.LanguageForFile(path)
		if lang == parser.LanguageUnknown || !ext.Supports(lang) {
			skippedCount++
			continue
		}

		result, err := p.ParseFile(ctx, path)
		if err != nil {
			parseErrors = append(parseErrors, fmt.Sprintf("%s: %v", path, err))
			continue
		}

		fileIR, err := ext.Extract(result)
		result.Close()
		if err != nil {
			parseErrors = append(parseErrors, fmt.Sprintf("%s: %v", path, err))
			continue
		}

		fileIRs = append(fileIRs, fileIR)
	}

	// --- Build graph ---
	builder := graph.NewBuilder()
	g := builder.Build(fileIRs)
	stats := g.Stats()

	// --- Build retrieval engine ---
	engine := retrieval.Build(g, fileIRs, retrieval.DefaultTokenBudget)

	// --- Store state ---
	s.mu.Lock()
	s.engine = engine
	s.g = g
	s.rootDir = sc.RootDir()
	s.mu.Unlock()

	// --- Build response ---
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("✓ Project indexed: %s\n\n", sc.RootDir()))
	sb.WriteString(fmt.Sprintf("Files scanned:   %d\n", len(files)))
	sb.WriteString(fmt.Sprintf("Files indexed:   %d\n", len(fileIRs)))
	sb.WriteString(fmt.Sprintf("Files skipped:   %d (unsupported language)\n", skippedCount))
	sb.WriteString(fmt.Sprintf("Parse errors:    %d\n\n", len(parseErrors)))
	sb.WriteString(fmt.Sprintf("Graph nodes:     %d\n", stats.TotalNodes))
	sb.WriteString(fmt.Sprintf("Graph edges:     %d\n\n", stats.TotalEdges))

	// Node breakdown.
	sb.WriteString("Node types:\n")
	kinds := make([]string, 0, len(stats.ByKind))
	for k := range stats.ByKind {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		sb.WriteString(fmt.Sprintf("  %-14s %d\n", k, stats.ByKind[graph.NodeKind(k)]))
	}

	if len(scanWarnings) > 0 {
		sb.WriteString(fmt.Sprintf("\nScan warnings (%d):\n", len(scanWarnings)))
		for _, w := range scanWarnings {
			sb.WriteString(fmt.Sprintf("  %s\n", w))
		}
	}

	if len(parseErrors) > 0 {
		sb.WriteString(fmt.Sprintf("\nParse errors (%d):\n", len(parseErrors)))
		for _, e := range parseErrors[:min(len(parseErrors), 5)] {
			sb.WriteString(fmt.Sprintf("  %s\n", e))
		}
		if len(parseErrors) > 5 {
			sb.WriteString(fmt.Sprintf("  ... and %d more\n", len(parseErrors)-5))
		}
	}

	sb.WriteString("\nReady for queries. Call query_context, find_symbol, find_callers, or trace_deps.")

	return sb.String(), nil
}

// =========================================================
// Tool: query_context
// =========================================================

type queryContextArgs struct {
	// Natural language description of what you're looking for.
	Query string `json:"query"`

	// Maximum tokens to return.
	TokenBudget int `json:"token_budget"`

	// Include debug scoring details.
	Debug bool `json:"debug"`
}

func (s *mearchServer) registerQueryContext(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name: "query_context",
		Description: `Retrieve the most relevant code context for a natural language query.

Given a description of what you're trying to do or understand, returns
the most relevant symbols, functions, types, and files from the indexed
codebase. Results are ranked by relevance and compressed to fit within
a token budget.

Use this as your primary tool for understanding code before making changes.
Call index_project first if you haven't already.

Examples:
  "scanner ignore rules"
  "how does the parser handle errors"
  "find the graph traversal logic"
  "authentication and session handling"`,
	}, func(ctx context.Context, req *sdk.CallToolRequest, args queryContextArgs) (*sdk.CallToolResult, any, error) {
		result, err := s.queryContext(args)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return textResult(result), nil, nil
	})
}

func (s *mearchServer) queryContext(args queryContextArgs) (string, error) {
	s.mu.RLock()
	engine := s.engine
	s.mu.RUnlock()

	if engine == nil {
		return "", errors.New("project not indexed — call index_project first")
	}

	if args.Query == "" {
		return "", errors.New("query is required")
	}

	tokenBudget := args.TokenBudget
	if tokenBudget <= 0 {
		tokenBudget = retrieval.DefaultTokenBudget
	}

	// Run the query.
	result := engine.Query(args.Query)

	if len(result.Nodes) == 0 && len(result.CollapsedFiles) == 0 {
		return fmt.Sprintf(
			"No results found for query: %q\n\nExpanded tokens: %v\nSeeds detected: %d\nCandidates explored: %d\n\nTry a more specific query or check that the project is indexed.",
			args.Query,
			result.ExpandedTokens,
			len(result.Seeds),
			result.CandidateCount,
		), nil
	}

	var sb strings.Builder

	// Header.
	sb.WriteString(fmt.Sprintf("Query: %q\n", result.Query))
	sb.WriteString(fmt.Sprintf("Token estimate: ~%d / %d\n", result.TokenEstimate, tokenBudget))
	sb.WriteString(fmt.Sprintf("Results: %d nodes, %d files\n", len(result.Nodes), len(result.Files)))

	if len(result.ExpandedTokens) > 0 {
		sb.WriteString(fmt.Sprintf("Expanded query: %v\n", result.ExpandedTokens))
	}

	// Seeds — what the engine latched onto.
	if len(result.Seeds) > 0 {
		sb.WriteString(fmt.Sprintf("\nDetected seeds (%d):\n", len(result.Seeds)))
		for _, seed := range result.Seeds {
			sb.WriteString(fmt.Sprintf("  [%.2f] %s\n", seed.Score, seed.NodeID))
		}
	}

	// Results.
	sb.WriteString(fmt.Sprintf("\nRelevant symbols (%d):\n", len(result.Nodes)))
	for i, rn := range result.Nodes {
		exported := " "
		if rn.Node.Visibility == graph.VisibilityExported {
			exported = "✓"
		}

		sb.WriteString(fmt.Sprintf("\n%d. %s [%s] %s\n",
			i+1,
			rn.Node.ID,
			rn.Node.Kind,
			exported,
		))

		if rn.Node.FilePath != "" {
			sb.WriteString(fmt.Sprintf("   file: %s\n", rn.Node.FilePath))
		}
		if rn.Node.Package != "" {
			sb.WriteString(fmt.Sprintf("   package: %s\n", rn.Node.Package))
		}
		sb.WriteString(fmt.Sprintf("   score: %.2f  depth: %d  via: %s\n",
			rn.Score, rn.Depth, rn.Direction))

		if args.Debug {
			sb.WriteString(fmt.Sprintf("   debug: %s\n", rn.Reason))
		}
	}

	// Collapsed files.
	if len(result.CollapsedFiles) > 0 {
		fmt.Fprintf(&sb, "\nHigh-relevance files (read these entirely):\n")
		for filePath, symbols := range result.CollapsedFiles {
			fmt.Fprintf(&sb, "  %s\n", filePath)
			fmt.Fprintf(&sb, "    relevant symbols: %s\n", strings.Join(symbols, ", "))
		}
	}

	// Files to read.
	if len(result.Files) > 0 {
		fmt.Fprintf(&sb, "\nFiles containing results:\n")
		for _, f := range result.Files {
			fmt.Fprintf(&sb, "  %s\n", f)
		}
	}

	return sb.String(), nil
}

// =========================================================
// Tool: find_symbol
// =========================================================

type findSymbolArgs struct {
	// Symbol name to find (partial match supported e.g. 'Scanner' or 'Scan').
	Name string `json:"name"`

	// Filter by node kind: function method struct interface const var file package (optional).
	Kind string `json:"kind"`

	// Filter by package name (optional).
	Package string `json:"package"`
}

func (s *mearchServer) registerFindSymbol(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name: "find_symbol",
		Description: `Find a specific symbol in the indexed codebase by name.

Searches for functions, methods, structs, interfaces, constants, and
variables by name. Supports partial matching — searching for "Scanner"
will find "scanner.Scanner", "scanner.NewScanner", etc.

Returns the symbol's location, kind, visibility, and graph connections.`,
	}, func(ctx context.Context, req *sdk.CallToolRequest, args findSymbolArgs) (*sdk.CallToolResult, any, error) {
		result, err := s.findSymbol(args)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return textResult(result), nil, nil
	})
}

func (s *mearchServer) findSymbol(args findSymbolArgs) (string, error) {
	s.mu.RLock()
	g := s.g
	s.mu.RUnlock()

	if g == nil {
		return "", errors.New("project not indexed — call index_project first")
	}
	if args.Name == "" {
		return "", errors.New("name is required")
	}

	searchLower := strings.ToLower(args.Name)
	var matches []*graph.Node

	for _, node := range g.AllNodes() {
		// Filter by kind if specified.
		if args.Kind != "" && string(node.Kind) != args.Kind {
			continue
		}
		// Filter by package if specified.
		if args.Package != "" && node.Package != args.Package {
			continue
		}
		// Skip external nodes.
		if node.Kind == graph.NodeKindExternal {
			continue
		}
		// Partial name match.
		if strings.Contains(strings.ToLower(node.Name), searchLower) ||
			strings.Contains(strings.ToLower(node.ID), searchLower) {
			matches = append(matches, node)
		}
	}

	if len(matches) == 0 {
		return fmt.Sprintf("No symbols found matching %q\n\nTry a shorter search term or check the spelling.", args.Name), nil
	}

	// Sort by ID for consistent output.
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].ID < matches[j].ID
	})

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d symbol(s) matching %q:\n", len(matches), args.Name))

	for _, node := range matches {
		sb.WriteString(fmt.Sprintf("\n%s\n", node.ID))
		sb.WriteString(fmt.Sprintf("  kind:       %s\n", node.Kind))
		sb.WriteString(fmt.Sprintf("  package:    %s\n", node.Package))
		sb.WriteString(fmt.Sprintf("  visibility: %s\n", node.Visibility))
		if node.FilePath != "" {
			sb.WriteString(fmt.Sprintf("  file:       %s\n", node.FilePath))
		}

		// Show connections.
		outEdges := g.OutEdges(node.ID)
		inEdges := g.InEdges(node.ID)

		if len(outEdges) > 0 {
			sb.WriteString(fmt.Sprintf("  outgoing edges (%d):\n", len(outEdges)))
			for _, e := range outEdges[:min(len(outEdges), 5)] {
				sb.WriteString(fmt.Sprintf("    -[%s]→ %s\n", e.Kind, e.To))
			}
			if len(outEdges) > 5 {
				sb.WriteString(fmt.Sprintf("    ... and %d more\n", len(outEdges)-5))
			}
		}

		if len(inEdges) > 0 {
			sb.WriteString(fmt.Sprintf("  incoming edges (%d):\n", len(inEdges)))
			for _, e := range inEdges[:min(len(inEdges), 5)] {
				sb.WriteString(fmt.Sprintf("    ←[%s] %s\n", e.Kind, e.From))
			}
			if len(inEdges) > 5 {
				sb.WriteString(fmt.Sprintf("    ... and %d more\n", len(inEdges)-5))
			}
		}
	}

	return sb.String(), nil
}

// =========================================================
// Tool: find_callers
// =========================================================

type findCallersArgs struct {
	// Symbol to find callers of.
	Symbol string `json:"symbol"`
}

func (s *mearchServer) registerFindCallers(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name: "find_callers",
		Description: `Find all code that calls a specific function or method.

Returns every function and file in the codebase that calls the
specified symbol. Useful for understanding the impact of changing
a function's signature or behavior.

Example: find_callers("Scanner.Scan") returns all code that calls Scan.`,
	}, func(ctx context.Context, req *sdk.CallToolRequest, args findCallersArgs) (*sdk.CallToolResult, any, error) {
		result, err := s.findCallers(args)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return textResult(result), nil, nil
	})
}

func (s *mearchServer) findCallers(args findCallersArgs) (string, error) {
	s.mu.RLock()
	g := s.g
	s.mu.RUnlock()

	if g == nil {
		return "", errors.New("project not indexed — call index_project first")
	}
	if args.Symbol == "" {
		return "", errors.New("symbol is required")
	}

	// Find matching nodes.
	searchLower := strings.ToLower(args.Symbol)
	var targets []*graph.Node
	for _, node := range g.AllNodes() {
		if node.Kind == graph.NodeKindExternal {
			continue
		}
		if strings.Contains(strings.ToLower(node.ID), searchLower) {
			targets = append(targets, node)
		}
	}

	if len(targets) == 0 {
		return fmt.Sprintf("No symbols found matching %q", args.Symbol), nil
	}

	var sb strings.Builder

	for _, target := range targets {
		callers := g.Dependents(target.ID, graph.EdgeKindCall)

		sb.WriteString(fmt.Sprintf("Callers of %s (%d):\n", target.ID, len(callers)))

		if len(callers) == 0 {
			sb.WriteString("  No callers found in the indexed codebase.\n")
			continue
		}

		for _, caller := range callers {
			sb.WriteString(fmt.Sprintf("  %s\n", caller.ID))
			if caller.FilePath != "" {
				sb.WriteString(fmt.Sprintf("    file: %s\n", caller.FilePath))
			}
		}
		sb.WriteString("\n")
	}

	return sb.String(), nil
}

// =========================================================
// Tool: trace_deps
// =========================================================

type traceDepsArgs struct {
	// Fully or partially qualified symbol name to trace dependencies for.
	Symbol string `json:"symbol"`

	// How many hops to follow (default 2 max 5).
	Depth int `json:"depth"`
}

func (s *mearchServer) registerTraceDeps(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name: "trace_deps",
		Description: `Trace the dependency chain of a symbol.

Follows outgoing edges from the symbol up to the specified depth,
showing what it imports, calls, and uses. Useful for understanding
what code a symbol depends on before modifying it.

Example: trace_deps("Parser.ParseFile") shows everything ParseFile depends on.`,
	}, func(ctx context.Context, req *sdk.CallToolRequest, args traceDepsArgs) (*sdk.CallToolResult, any, error) {
		result, err := s.traceDeps(args)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return textResult(result), nil, nil
	})
}

func (s *mearchServer) traceDeps(args traceDepsArgs) (string, error) {
	s.mu.RLock()
	g := s.g
	s.mu.RUnlock()

	if g == nil {
		return "", errors.New("project not indexed — call index_project first")
	}
	if args.Symbol == "" {
		return "", errors.New("symbol is required")
	}

	depth := args.Depth
	if depth <= 0 {
		depth = 2
	}
	if depth > 5 {
		depth = 5
	}

	// Find the starting node.
	searchLower := strings.ToLower(args.Symbol)
	var startNode *graph.Node
	for _, node := range g.AllNodes() {
		if node.Kind == graph.NodeKindExternal {
			continue
		}
		if strings.Contains(strings.ToLower(node.ID), searchLower) {
			startNode = node
			break
		}
	}

	if startNode == nil {
		return fmt.Sprintf("No symbol found matching %q", args.Symbol), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Dependency trace for %s (depth %d):\n\n", startNode.ID, depth))

	// BFS traversal collecting dependencies.
	g.BFS(startNode.ID, depth, func(node *graph.Node, d int) bool {
		if node.Kind == graph.NodeKindExternal {
			return true // visit but don't expand
		}
		indent := strings.Repeat("  ", d)
		sb.WriteString(fmt.Sprintf("%s[depth %d] %s (%s)\n", indent, d, node.ID, node.Kind))
		return true
	})

	return sb.String(), nil
}

// =========================================================
// Tool: graph_stats
// =========================================================

type graphStatsArgs struct{}

func (s *mearchServer) registerGraphStats(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name: "graph_stats",
		Description: `Return statistics about the indexed project graph.

Shows the number of nodes, edges, and breakdown by node type.
Useful for understanding the scale of an indexed project and
verifying the index is complete.`,
	}, func(ctx context.Context, req *sdk.CallToolRequest, args graphStatsArgs) (*sdk.CallToolResult, any, error) {
		result, err := s.graphStats()
		if err != nil {
			return errorResult(err), nil, nil
		}
		return textResult(result), nil, nil
	})
}

func (s *mearchServer) graphStats() (string, error) {
	s.mu.RLock()
	g := s.g
	rootDir := s.rootDir
	s.mu.RUnlock()

	if g == nil {
		return "", errors.New("project not indexed — call index_project first")
	}

	stats := g.Stats()

	var sb strings.Builder
	fmt.Fprintf(&sb, "Mearch index stats\n")
	fmt.Fprintf(&sb, "project: %s\n\n", rootDir)
	fmt.Fprintf(&sb, "total nodes: %d\n", stats.TotalNodes)
	fmt.Fprintf(&sb, "total edges: %d\n\n", stats.TotalEdges)
	fmt.Fprintf(&sb, "nodes by kind:\n")

	kinds := make([]string, 0, len(stats.ByKind))
	for k := range stats.ByKind {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Fprintf(&sb, "  %-14s %d\n", k, stats.ByKind[graph.NodeKind(k)])
	}

	return sb.String(), nil
}

// =========================================================
// Helpers
// =========================================================

// textResult wraps a string as an MCP text content result.
func textResult(text string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		Content: []sdk.Content{
			&sdk.TextContent{Text: text},
		},
	}
}

// errorResult wraps an error as an MCP error result.
func errorResult(err error) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{
			&sdk.TextContent{Text: "error: " + err.Error()},
		},
	}
}

// jsonResult serializes a value as JSON and wraps it as an MCP text result.
// Used when structured data is more useful than formatted text.
func jsonResult(v any) *sdk.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errorResult(fmt.Errorf("failed to serialize result: %w", err))
	}
	return textResult(string(b))
}

// min returns the smaller of two integers.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// keep jsonResult available for future tools
var _ = jsonResult
