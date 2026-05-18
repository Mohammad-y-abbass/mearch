package scanner

// ignoredDirs contains directory names that should never be traversed.
//
// These are matched against the directory's base name only (not full path),
// so "project/node_modules" and "project/packages/app/node_modules" are
// both caught by a single "node_modules" entry.
//
// Using a map gives O(1) lookup — important when walking large repos where
// this check fires on every directory entry.
var ignoredDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	"dist":         true,
	"build":        true,
	"coverage":     true,
	".next":        true,
	"out":          true,
	"vendor":       true,
	".cache":       true,
	"__pycache__":  true,
	".turbo":       true,
	".svelte-kit":  true,
}

