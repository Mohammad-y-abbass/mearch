package scanner

// supportedExtensions contains file extensions Mearch can parse.
//
// Extensions are stored with the leading dot (e.g. ".go") to match
// filepath.Ext output directly — no string manipulation needed at
// call time.
//
// Add new languages here as Tree-sitter grammars are integrated.
// Do NOT add extensions without a corresponding parser implementation —
// the scanner and parser layers must stay in sync.
var supportedExtensions = map[string]bool{
	// Phase 1: initial language support
	".go":  true,
	".mod": true,
	".sum": true,

	// planned language support (uncomment as parsers land)
	// ".c":      true,
	// ".cpp":    true,
	// ".js":     true,
	// ".ts":     true,
	// ".jsx":    true,
	// ".tsx":    true,
	// ".rs":     true,
	// ".py":     true,
	// ".java":   true,
	// ".kt":     true,
	// ".swift":  true,
}
