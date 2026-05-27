// Package scanner implements the filesystem discovery layer for Mearch.
//
// The scanner is responsible for one thing only: finding files that are
// eligible for parsing. It does NOT parse, extract IR, or build graphs.
// Keeping this boundary clean is critical — the scanner is stateless and
// should remain that way.
//
// Architecture position:
//
//	Filesystem → [Scanner] → Parser → IR → Graph → Retrieval
package scanner

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

// Scanner discovers source files within a directory tree.
//
// Scanner is safe for concurrent use. All configuration is read-only
// after construction — do not mutate fields after calling NewScanner.
type Scanner struct {
	// rootDir is the absolute path to scan from.
	// Stored as absolute to ensure consistent path output regardless of
	// the working directory at scan time.
	rootDir string

	// ignoredDirs is the effective set of ignored directory names,
	// combining the package-level defaults with any ExtraIgnoredDirs.
	ignoredDirs map[string]bool

	// extensions is the effective set of supported file extensions,
	// combining the package-level defaults with any ExtraExtensions.
	extensions map[string]bool

	// opts holds the resolved scan options after construction.
	opts ScanOptions
}

// NewScanner constructs a Scanner rooted at rootDir.
//
// rootDir is resolved to an absolute path at construction time.
// Returns an error if rootDir does not exist or is not a directory.
//
// Example:
//
//	s, err := NewScanner("./myproject", ScanOptions{})
func NewScanner(rootDir string, opts ScanOptions) (*Scanner, error) {
	// Resolve to absolute path immediately so all downstream paths are
	// stable and predictable regardless of cwd changes.
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}

	// Validate the root exists and is a directory before doing anything else.
	// Failing fast here produces a clear error rather than a silent empty scan.
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &NotADirectoryError{Path: abs}
	}

	// Build the effective ignored dirs set by merging defaults with extras.
	// We copy the package-level map rather than mutating it — the package-
	// level map is shared across all Scanner instances.
	effective_ignored := make(map[string]bool, len(ignoredDirs)+len(opts.ExtraIgnoredDirs))

	maps.Copy(effective_ignored, ignoredDirs)

	for _, d := range opts.ExtraIgnoredDirs {
		effective_ignored[d] = true
	}

	// Same merge pattern for extensions.
	effectiveExts := make(map[string]bool, len(supportedExtensions)+len(opts.ExtraExtensions))

	maps.Copy(effectiveExts, supportedExtensions)

	for _, ext := range opts.ExtraExtensions {
		// Normalise: ensure the leading dot is present so callers don't
		// have to remember to include it.
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		effectiveExts[strings.ToLower(ext)] = true
	}

	return &Scanner{
		rootDir:     abs,
		ignoredDirs: effective_ignored,
		extensions:  effectiveExts,
		opts:        opts,
	}, nil
}

// Scan walks the directory tree rooted at s.rootDir and returns the absolute
// paths of all files eligible for parsing.
//
// Files are returned in filesystem walk order (lexicographic within each
// directory). Callers should not assume any other ordering.
//
// Scan is non-destructive and read-only. It will never modify, delete, or
// execute any file it encounters.
//
// Error behaviour:
//   - If a file or directory cannot be stat'd, the error is collected and
//     the walk continues. All collected errors are returned as a ScanError
//     after the walk completes so callers get the full picture rather than
//     failing on the first unreadable path.
//   - If the root itself cannot be walked, a non-nil error is returned
//     immediately.
func (s *Scanner) Scan() ([]string, error) {
	// Pre-allocate with a reasonable capacity to avoid repeated slice growth
	// for typical repository sizes. 512 is a conservative lower bound for
	// most real projects — the slice will grow automatically if needed.
	files := make([]string, 0, 512)

	// Collect non-fatal errors (e.g. permission denied on a single file)
	// rather than aborting the entire scan. This matches the principle of
	// maximum useful output — a partially-indexed repo is better than none.
	var scanErrs ScanErrors

	err := filepath.WalkDir(s.rootDir, func(path string, d fs.DirEntry, err error) error {
		// err is non-nil when the OS failed to read this entry (permission
		// denied, broken symlink, etc). Collect it and keep walking.
		if err != nil {
			scanErrs = append(scanErrs, &ScanError{Path: path, Err: err})
			if d != nil && d.IsDir() {
				// Can't read the directory — skip it entirely.
				return filepath.SkipDir
			}
			return nil
		}

		// --- Directory handling ---
		if d.IsDir() {
			// Always allow the root directory itself through; the ignore
			// rules apply to subdirectories only.
			if path == s.rootDir {
				return nil
			}

			// Enforce MaxDepth if configured.
			// Depth is the number of path separators between root and path.
			if s.opts.MaxDepth > 0 {
				rel, _ := filepath.Rel(s.rootDir, path)
				depth := strings.Count(rel, string(filepath.Separator)) + 1
				if depth > s.opts.MaxDepth {
					return filepath.SkipDir
				}
			}

			// Check ignore list by base name only.
			// This intentionally ignores the full path so that deeply nested
			// copies of ignored directories (e.g. packages/app/node_modules)
			// are caught by the same rule as top-level ones.
			if s.ignoredDirs[d.Name()] {
				return filepath.SkipDir
			}

			// Symlink handling for directories.
			// filepath.WalkDir does NOT follow symlinks by default.
			// When FollowSymlinks is false (default), we explicitly skip
			// symlinked directories to avoid cycles.
			//
			// TODO: When FollowSymlinks is true, add inode-based cycle
			// detection here before following any symlink.
			if !s.opts.FollowSymlinks {
				info, err := d.Info()
				if err == nil && info.Mode()&os.ModeSymlink != 0 {
					return filepath.SkipDir
				}
			}

			return nil
		}

		// --- File handling ---

		// Skip symlinked files when FollowSymlinks is false.
		// We still want to index real files even if we're not following
		// symlinked directories.
		if !s.opts.FollowSymlinks {
			info, err := d.Info()
			if err == nil && info.Mode()&os.ModeSymlink != 0 {
				return nil
			}
		}

		// Check extension eligibility.
		// filepath.Ext returns the last dot-delimited suffix including the dot,
		// or "" for files with no extension. We lowercase for case-insensitive
		// matching (relevant on case-insensitive filesystems like macOS HFS+).
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext == "" || !s.extensions[ext] {
			return nil
		}

		files = append(files, path)
		return nil
	})

	// A non-nil err here means the root walk itself failed — that's fatal.
	if err != nil {
		return nil, err
	}

	// Return collected non-fatal errors alongside results.
	// Callers can check scanErrs == nil to determine if the scan was clean.
	if len(scanErrs) > 0 {
		return files, scanErrs
	}

	return files, nil
}

// RootDir returns the absolute root path this scanner is configured for.
func (s *Scanner) RootDir() string {
	return s.rootDir
}

// IsSupported reports whether the given file path has a supported extension.
//
// This is exposed for use by other packages (e.g. the watcher) that need to
// check individual files without running a full scan.
func (s *Scanner) IsSupported(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext != "" && s.extensions[ext]
}
