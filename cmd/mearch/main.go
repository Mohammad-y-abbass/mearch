package main

import (
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/mohamamd-y-abbass/mearch/internal/scanner"
)

func main() {
	// Get target directory from CLI arg, default to current directory.
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}

	s, err := scanner.NewScanner(root, scanner.ScanOptions{
		// Test extra ignore dirs
		ExtraIgnoredDirs: []string{"testdata"},
		// Test extra extensions
		ExtraExtensions: []string{".json"},
		// Limit depth so output stays readable during testing
		MaxDepth: 5,
	})
	if err != nil {
		log.Fatalf("failed to create scanner: %v", err)
	}

	fmt.Printf("scanning: %s\n\n", s.RootDir())

	files, err := s.Scan()

	// Handle non-fatal scan errors — print them but don't stop.
	var scanErrs scanner.ScanErrors
	if errors.As(err, &scanErrs) {
		fmt.Println("=== scan warnings ===")
		for _, e := range scanErrs {
			fmt.Printf("  skipped: %v\n", e)
		}
		fmt.Println()
	} else if err != nil {
		// Fatal error — the root walk itself failed.
		log.Fatalf("scan failed: %v", err)
	}

	// Print results.
	fmt.Printf("=== found %d files ===\n", len(files))
	for _, f := range files {
		fmt.Println(" ", f)
	}
}
