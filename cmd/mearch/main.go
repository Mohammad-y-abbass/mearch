package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/mohamamd-y-abbass/mearch/internal/extractor"
	"github.com/mohamamd-y-abbass/mearch/internal/parser"
	"github.com/mohamamd-y-abbass/mearch/internal/scanner"
)

func main() {
	// Get target directory from CLI arg, default to current directory.
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}

	// Optional flag: pass "--json" as second arg to dump full IR as JSON.
	dumpJSON := len(os.Args) > 2 && os.Args[2] == "--json"

	// =========================================================
	// STEP 1: SCAN
	// =========================================================
	fmt.Println("╔══════════════════════════════╗")
	fmt.Println("║          SCANNER             ║")
	fmt.Println("╚══════════════════════════════╝")

	s, err := scanner.NewScanner(root, scanner.ScanOptions{
		MaxDepth: 10,
	})
	if err != nil {
		log.Fatalf("scanner init failed: %v", err)
	}

	fmt.Printf("root: %s\n\n", s.RootDir())

	files, err := s.Scan()

	// Non-fatal scan errors — log and continue.
	var scanErrs scanner.ScanErrors
	if errors.As(err, &scanErrs) {
		fmt.Println("⚠ scan warnings:")
		for _, e := range scanErrs {
			fmt.Printf("  skipped: %v\n", e)
		}
		fmt.Println()
	} else if err != nil {
		log.Fatalf("scan failed: %v", err)
	}

	fmt.Printf("✓ found %d files\n\n", len(files))

	// =========================================================
	// STEP 2: PARSE + EXTRACT IR
	// =========================================================
	fmt.Println("╔══════════════════════════════╗")
	fmt.Println("║       PARSER + EXTRACTOR     ║")
	fmt.Println("╚══════════════════════════════╝")

	p := parser.NewParser()
	defer p.Close()

	ext := extractor.NewGoExtractor()

	ctx := context.Background()

	var (
		totalParsed  int
		totalSkipped int
		totalErrored int
	)

	for _, path := range files {
		// Skip files with no registered grammar yet.
		if parser.LanguageForFile(path) == parser.LanguageUnknown {
			totalSkipped++
			continue
		}

		// --- Parse ---
		result, err := p.ParseFile(ctx, path)
		if err != nil {
			fmt.Printf("✗ [PARSE ERROR] %s\n  %v\n\n", path, err)
			totalErrored++
			continue
		}
		defer result.Close()

		// Warn about syntax errors but keep going — partial IR is fine.
		if result.HasErrors() {
			fmt.Printf("⚠ [SYNTAX ERRORS] %s — extracting partial IR\n", path)
		}

		// --- Extract IR ---
		fileIR, err := ext.Extract(result)
		if err != nil {
			fmt.Printf("✗ [IR ERROR] %s\n  %v\n\n", path, err)
			totalErrored++
			continue
		}

		totalParsed++

		// --- Print IR summary ---
		fmt.Printf("✓ %s\n", path)
		fmt.Printf("  package   : %s\n", fileIR.Package)
		fmt.Printf("  imports   : %d\n", len(fileIR.Imports))
		fmt.Printf("  functions : %d\n", len(fileIR.Functions))
		fmt.Printf("  symbols   : %d\n", len(fileIR.Symbols))
		fmt.Printf("  calls     : %d\n", len(fileIR.Calls))
		fmt.Printf("  edges     : %d\n", len(fileIR.Edges))

		// Imports
		if len(fileIR.Imports) > 0 {
			fmt.Println("  ┌─ imports")
			for _, imp := range fileIR.Imports {
				alias := ""
				if imp.Alias != "" {
					alias = fmt.Sprintf(" (alias: %s)", imp.Alias)
				}
				fmt.Printf("  │  %s%s\n", imp.Path, alias)
			}
		}

		// Functions and methods
		if len(fileIR.Functions) > 0 {
			fmt.Println("  ┌─ functions")
			for _, fn := range fileIR.Functions {
				receiver := ""
				if fn.Receiver != "" {
					receiver = fmt.Sprintf(" [receiver: %s]", fn.Receiver)
				}
				fmt.Printf("  │  %-40s %s%s\n", fn.Qualified, fn.Visibility, receiver)
			}
		}

		// Symbols (structs, interfaces, etc.)
		if len(fileIR.Symbols) > 0 {
			fmt.Println("  ┌─ symbols")
			for _, sym := range fileIR.Symbols {
				fmt.Printf("  │  %-40s kind=%-10s %s\n", sym.Qualified, sym.Kind, sym.Visibility)
			}
		}

		// Calls
		if len(fileIR.Calls) > 0 {
			fmt.Println("  ┌─ calls")
			for _, call := range fileIR.Calls {
				fmt.Printf("  │  %-40s kind=%s\n", call.Target, call.Kind)
			}
		}

		// Edges
		if len(fileIR.Edges) > 0 {
			fmt.Println("  ┌─ edges")
			for _, edge := range fileIR.Edges {
				fmt.Printf("  │  %-30s -[%-10s]→ %s\n", edge.From, edge.Kind, edge.To)
			}
		}

		// Full JSON dump if --json flag passed
		if dumpJSON {
			fmt.Println("  ┌─ full IR (JSON)")
			b, _ := json.MarshalIndent(fileIR, "  ", "  ")
			fmt.Println(string(b))
		}

		fmt.Println()
	}

	// =========================================================
	// SUMMARY
	// =========================================================
	fmt.Println("╔══════════════════════════════╗")
	fmt.Println("║           SUMMARY            ║")
	fmt.Println("╚══════════════════════════════╝")
	fmt.Printf("  ✓ extracted : %d files\n", totalParsed)
	fmt.Printf("  ⊘ skipped   : %d files (unsupported language)\n", totalSkipped)
	fmt.Printf("  ✗ errors    : %d files\n", totalErrored)
}
