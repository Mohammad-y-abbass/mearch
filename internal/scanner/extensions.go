package scanner

// supportedExtensions contains file extensions Mearch can index.
//
// Extensions are stored with the leading dot (e.g. ".go") to match
// filepath.Ext output directly, so no string manipulation is needed at call time.
//
// Add new languages here after both parser and extractor support exist. The
// parser may understand more grammars than the extractor indexes; the scanner
// should only surface files that can become graph IR.
var supportedExtensions = map[string]bool{
	// Go
	".go":  true,
	".mod": true,
	".sum": true,

	// Python
	".py": true,

	// JavaScript / JSX
	".js":  true,
	".jsx": true,
	".mjs": true,
	".cjs": true,

	// TypeScript / TSX
	".ts":  true,
	".tsx": true,

	// Rust
	".rs": true,

	// Java
	".java": true,
}
