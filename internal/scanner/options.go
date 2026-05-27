package scanner

// ScanOptions configures scanner behaviour.
//
// The zero value is safe to use and applies sensible defaults.
// This struct is intentionally kept flat — avoid nesting options
// inside sub-structs until there is a clear need.
type ScanOptions struct {
	// ExtraIgnoredDirs extends the default ignoredDirs set.
	// Useful for project-specific directories like "generated/" or ".yarn/".
	// Values are matched against directory base names, not full paths.
	ExtraIgnoredDirs []string

	// ExtraExtensions extends the default supportedExtensions set.
	// Include the leading dot: ".vue", not "vue".
	ExtraExtensions []string

	// MaxDepth limits traversal depth. 0 means unlimited.
	// Useful for shallow scans during testing or initial indexing previews.
	// Depth is counted from RootDir (depth 1 = direct children of root).
	MaxDepth int

	// FollowSymlinks controls whether symbolic links to directories are
	// traversed. Disabled by default because symlink cycles can cause
	// infinite walks and are common in monorepo workspace setups.
	//
	// TODO: When enabled, implement cycle detection via inode tracking
	// before shipping this to production. Symlink loops will hang the
	// indexer indefinitely without it.
	FollowSymlinks bool
}
