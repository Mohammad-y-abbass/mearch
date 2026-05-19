package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"

	"github.com/mohamamd-y-abbass/mearch/internal/extractor"
	"github.com/mohamamd-y-abbass/mearch/internal/graph"
	"github.com/mohamamd-y-abbass/mearch/internal/ir"
	"github.com/mohamamd-y-abbass/mearch/internal/parser"
	"github.com/mohamamd-y-abbass/mearch/internal/scanner"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}

	// =========================================================
	// STEP 1: SCAN
	// =========================================================
	printHeader("1. SCANNER")

	s, err := scanner.NewScanner(root, scanner.ScanOptions{MaxDepth: 10})
	if err != nil {
		log.Fatalf("scanner init failed: %v", err)
	}
	fmt.Printf("  root: %s\n\n", s.RootDir())

	files, err := s.Scan()
	var scanErrs scanner.ScanErrors
	if errors.As(err, &scanErrs) {
		for _, e := range scanErrs {
			fmt.Printf("  ⚠ skipped: %v\n", e)
		}
	} else if err != nil {
		log.Fatalf("scan failed: %v", err)
	}
	fmt.Printf("  ✓ %d files found\n\n", len(files))

	// =========================================================
	// STEP 2: PARSE + EXTRACT IR
	// =========================================================
	printHeader("2. PARSER + IR EXTRACTOR")

	p := parser.NewParser()
	defer p.Close()

	ext := extractor.NewGoExtractor()
	ctx := context.Background()

	var (
		fileIRs      []*ir.FileIR
		totalSkipped int
		totalErrored int
	)

	for _, path := range files {
		if parser.LanguageForFile(path) == parser.LanguageUnknown {
			totalSkipped++
			continue
		}

		result, err := p.ParseFile(ctx, path)
		if err != nil {
			fmt.Printf("  ✗ [PARSE ERROR] %s: %v\n", path, err)
			totalErrored++
			continue
		}
		defer result.Close()

		if result.HasErrors() {
			fmt.Printf("  ⚠ [SYNTAX ERRORS] %s\n", path)
		}

		fileIR, err := ext.Extract(result)
		if err != nil {
			fmt.Printf("  ✗ [IR ERROR] %s: %v\n", path, err)
			totalErrored++
			continue
		}

		fileIRs = append(fileIRs, fileIR)
		fmt.Printf("  ✓ %-60s pkg=%-15s fn=%-3d sym=%-3d imports=%d\n",
			shorten(path, 60),
			fileIR.Package,
			len(fileIR.Functions),
			len(fileIR.Symbols),
			len(fileIR.Imports),
		)
	}

	fmt.Printf("\n  extracted: %d  skipped: %d  errors: %d\n\n",
		len(fileIRs), totalSkipped, totalErrored)

	// =========================================================
	// STEP 3: BUILD GRAPH
	// =========================================================
	printHeader("3. GRAPH BUILDER")

	builder := graph.NewBuilder()
	g := builder.Build(fileIRs)
	stats := g.Stats()

	fmt.Printf("  ✓ graph built\n\n")
	fmt.Printf("  total nodes : %d\n", stats.TotalNodes)
	fmt.Printf("  total edges : %d\n", stats.TotalEdges)
	fmt.Println()
	fmt.Println("  nodes by kind:")

	// Print node kinds in a stable sorted order.
	kinds := make([]string, 0, len(stats.ByKind))
	for k := range stats.ByKind {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Printf("    %-14s %d\n", k, stats.ByKind[graph.NodeKind(k)])
	}

	// =========================================================
	// STEP 4: SPOT CHECKS — traverse the graph
	// =========================================================
	printHeader("4. GRAPH TRAVERSAL SPOT CHECKS")

	allNodes := g.AllNodes()

	// --- 4a. Pick the first file node and show its imports ---
	var fileNode *graph.Node
	for _, n := range allNodes {
		if n.Kind == graph.NodeKindFile {
			fileNode = n
			break
		}
	}

	if fileNode != nil {
		fmt.Printf("  File node: %s\n", fileNode.ID)
		imports := g.Neighbors(fileNode.ID, graph.EdgeKindImport)
		fmt.Printf("  Imports (%d):\n", len(imports))
		for _, imp := range imports {
			fmt.Printf("    → %s\n", imp.ID)
		}
		fmt.Println()
	}

	// --- 4b. Pick the first struct node and show its methods ---
	var structNode *graph.Node
	for _, n := range allNodes {
		if n.Kind == graph.NodeKindStruct {
			structNode = n
			break
		}
	}

	if structNode != nil {
		fmt.Printf("  Struct node: %s\n", structNode.ID)
		methods := g.Neighbors(structNode.ID, graph.EdgeKindDefine)
		fmt.Printf("  Methods (%d):\n", len(methods))
		for _, m := range methods {
			fmt.Printf("    → %s\n", m.ID)
		}
		fmt.Println()
	}

	// --- 4c. BFS from first package node, depth 2 ---
	var pkgNode *graph.Node
	for _, n := range allNodes {
		if n.Kind == graph.NodeKindPackage {
			pkgNode = n
			break
		}
	}

	if pkgNode != nil {
		fmt.Printf("  BFS from package %q (max depth 2):\n", pkgNode.ID)
		g.BFS(pkgNode.ID, 2, func(n *graph.Node, depth int) bool {
			indent := "  "
			for i := 0; i < depth; i++ {
				indent += "  "
			}
			fmt.Printf("%s[depth %d] %-12s %s\n", indent, depth, n.Kind, n.ID)
			return true
		})
		fmt.Println()
	}

	// --- 4d. Reverse lookup — who depends on the first external package? ---
	var extNode *graph.Node
	for _, n := range allNodes {
		if n.Kind == graph.NodeKindExternal {
			extNode = n
			break
		}
	}

	if extNode != nil {
		fmt.Printf("  Reverse lookup — who imports %q?\n", extNode.ID)
		dependents := g.Dependents(extNode.ID, graph.EdgeKindImport)
		for _, d := range dependents {
			fmt.Printf("    ← %s (%s)\n", d.ID, d.Kind)
		}
		fmt.Println()
	}

	// =========================================================
	// SUMMARY
	// =========================================================
	printHeader("SUMMARY")
	fmt.Printf("  files scanned   : %d\n", len(files))
	fmt.Printf("  IRs extracted   : %d\n", len(fileIRs))
	fmt.Printf("  graph nodes     : %d\n", stats.TotalNodes)
	fmt.Printf("  graph edges     : %d\n", stats.TotalEdges)
	fmt.Printf("  skipped         : %d (unsupported language)\n", totalSkipped)
	fmt.Printf("  errors          : %d\n", totalErrored)
}

// =========================================================
// Helpers
// =========================================================

func printHeader(title string) {
	line := "═══════════════════════════════════════"
	fmt.Printf("╔%s╗\n", line)
	fmt.Printf("║  %-36s║\n", title)
	fmt.Printf("╚%s╝\n", line)
}

// shorten truncates a string from the left if it exceeds maxLen,
// preserving the rightmost characters (the filename part).
func shorten(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return "..." + s[len(s)-(maxLen-3):]
}
