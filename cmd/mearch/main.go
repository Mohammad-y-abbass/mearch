package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/mohamamd-y-abbass/mearch/internal/parser"
	"github.com/mohamamd-y-abbass/mearch/internal/scanner"
)

func main() {
	// Get target directory from CLI arg, default to current directory.
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}

	// --- Step 1: Scan ---
	fmt.Println("=== SCANNER ===")

	s, err := scanner.NewScanner(root, scanner.ScanOptions{
		MaxDepth: 10,
	})
	if err != nil {
		log.Fatalf("scanner init failed: %v", err)
	}

	fmt.Printf("root: %s\n\n", s.RootDir())

	files, err := s.Scan()

	// Non-fatal scan errors — print and continue.
	var scanErrs scanner.ScanErrors
	if errors.As(err, &scanErrs) {
		fmt.Println("scan warnings:")
		for _, e := range scanErrs {
			fmt.Printf("  skipped: %v\n", e)
		}
		fmt.Println()
	} else if err != nil {
		log.Fatalf("scan failed: %v", err)
	}

	fmt.Printf("found %d files\n\n", len(files))

	// --- Step 2: Parse each file ---
	fmt.Println("=== PARSER ===")

	// One parser instance for the whole run.
	// In production the indexer will create one per worker goroutine.
	p := parser.NewParser()
	defer p.Close()

	ctx := context.Background()

	var (
		parsed  int
		skipped int
		errored int
	)

	for _, path := range files {
		// Skip files the parser doesn't support yet (e.g. .ts, .tsx until
		// those grammars are wired in).
		if parser.LanguageForFile(path) == parser.LanguageUnknown {
			skipped++
			continue
		}

		result, err := p.ParseFile(ctx, path)
		if err != nil {
			fmt.Printf("  [ERROR]  %s\n    %v\n", path, err)
			errored++
			continue
		}

		// Always free the tree when done.
		// In the real indexer, the IR extractor will consume the tree
		// before Close() is called.
		defer result.Close()

		// Print a summary line per file.
		status := "OK"
		if result.HasErrors() {
			status = "PARSE ERRORS"
		}

		fmt.Printf("  [%-12s] %s\n", status, path)
		fmt.Printf("    language : %s\n", result.Language)
		fmt.Printf("    bytes    : %d\n", len(result.Source))
		fmt.Printf("    root     : %s\n", result.RootNode().Kind())

		// Print the top-level children of the tree — gives a quick feel
		// for what Tree-sitter sees without dumping the full S-expression.
		root := result.RootNode()
		childCount := root.NamedChildCount()
		fmt.Printf("    children : %d named top-level nodes\n", childCount)

		// Show up to 5 top-level nodes so output stays readable.
		limit := childCount
		if limit > 5 {
			limit = 5
		}
		for i := range limit {
			child := root.NamedChild(uint(i))
			if child == nil {
				continue
			}
			fmt.Printf("      [%d] %-20s %q\n",
				i,
				child.Kind(),
				truncate(result.NodeContent(child), 60),
			)
		}
		if childCount > 5 {
			fmt.Printf("      ... and %d more\n", childCount-5)
		}

		fmt.Println()
		parsed++
	}

	// --- Summary ---
	fmt.Println("=== SUMMARY ===")
	fmt.Printf("  parsed  : %d\n", parsed)
	fmt.Printf("  skipped : %d (unsupported language)\n", skipped)
	fmt.Printf("  errors  : %d\n", errored)
}

// truncate shortens a string to maxLen characters for display purposes.
// Adds "..." suffix when truncated.
func truncate(s string, maxLen int) string {
	// Strip newlines so multi-line nodes display on one line.
	out := ""
	for _, ch := range s {
		if ch == '\n' || ch == '\r' || ch == '\t' {
			out += " "
		} else {
			out += string(ch)
		}
	}
	if len(out) <= maxLen {
		return out
	}
	return out[:maxLen] + "..."
}
