package extractor

import (
	"strings"
	"unicode"

	"github.com/mohamamd-y-abbass/mearch/internal/ir"
)

// qualifiedName builds a stable fully-qualified symbol name.
//
//	qualifiedName("scanner", "",        "NewScanner") → "scanner.NewScanner"
//	qualifiedName("scanner", "Scanner", "Scan")       → "scanner.Scanner.Scan"
func qualifiedName(pkg, receiver, name string) string {
	if receiver != "" {
		return pkg + "." + receiver + "." + name
	}
	return pkg + "." + name
}

// visibility returns "exported" if name starts with an uppercase letter,
// "unexported" otherwise.
func visibility(name string) string {
	if name == "" {
		return "unexported"
	}
	for _, r := range name {
		if unicode.IsUpper(r) {
			return "exported"
		}
		return "unexported"
	}
	return "unexported"
}

// importPackageName returns the effective local name of an import.
//
// If an alias is set, that is the local name.
// Otherwise, the last path segment is used (standard Go convention).
//
//	"fmt"                                      → "fmt"
//	"github.com/yourorg/mearch/internal/parser" → "parser"
//	alias "fmt"                                → "alias"
func importPackageName(imp ir.ImportIR) string {
	if imp.Alias != "" && imp.Alias != "_" && imp.Alias != "." {
		return imp.Alias
	}
	parts := strings.Split(imp.Path, "/")
	return parts[len(parts)-1]
}
