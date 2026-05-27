package parser

import (
	"path/filepath"
	"strings"
)

// String implements fmt.Stringer for human-readable logging and error messages.
func (l Language) String() string {
	switch l {
	case LanguageGo:
		return "go"
	case LanguagePython:
		return "python"
	case LanguageJavaScript:
		return "javascript"
	case LanguageTypeScript:
		return "typescript"
	case LanguageTSX:
		return "tsx"
	case LanguageRust:
		return "rust"
	case LanguageC:
		return "c"
	case LanguageCPP:
		return "cpp"
	case LanguageJava:
		return "java"
	case LanguageBash:
		return "bash"
	case LanguageJSON:
		return "json"
	case LanguageHTML:
		return "html"
	case LanguageCSS:
		return "css"
	default:
		return "unknown"
	}
}

// languageForExt maps lowercase file extensions to their Language constant.
//
// This is the single source of truth for extension → language resolution
// in the parser layer. The scanner has a parallel extension list for
// file discovery — keep them in sync when adding new languages.
var languageForExt = map[string]Language{
	// Go
	".go": LanguageGo,

	// Python
	".py": LanguagePython,

	// JavaScript / JSX (both use the JS grammar)
	".js":  LanguageJavaScript,
	".jsx": LanguageJavaScript,
	".mjs": LanguageJavaScript,
	".cjs": LanguageJavaScript,

	// TypeScript
	".ts": LanguageTypeScript,

	// TypeScript JSX
	".tsx": LanguageTSX,

	// Rust
	".rs": LanguageRust,

	// C
	".c": LanguageC,
	".h": LanguageC,

	// C++
	".cpp": LanguageCPP,
	".cc":  LanguageCPP,
	".cxx": LanguageCPP,
	".hpp": LanguageCPP,
	".hxx": LanguageCPP,

	// Java
	".java": LanguageJava,

	// Bash / shell
	".sh":   LanguageBash,
	".bash": LanguageBash,

	// JSON
	".json": LanguageJSON,

	// HTML
	".html": LanguageHTML,
	".htm":  LanguageHTML,

	// CSS
	".css": LanguageCSS,
}

// LanguageForFile detects the Language for a given file path from its extension.
// Returns LanguageUnknown for unrecognised extensions.
//
// Exposed so other packages (watcher, indexer) can check language support
// without constructing a full Parser.
func LanguageForFile(path string) Language {
	ext := strings.ToLower(filepath.Ext(path))
	if lang, ok := languageForExt[ext]; ok {
		return lang
	}
	return LanguageUnknown
}

// Close releases the Tree-sitter C memory held by this ParseResult.
//
// Always call Close when done, using defer at the call site:
//
//	result, err := p.ParseFile(ctx, path)
//	if err != nil { return err }
//	defer result.Close()
//
// Calling Close on a nil ParseResult or after already closing is safe.
func (r *ParseResult) Close() {
	if r != nil && r.Tree != nil {
		r.Tree.Close()
		r.Tree = nil
	}
}
