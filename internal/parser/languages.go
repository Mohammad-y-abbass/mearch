package parser

// Language represents a supported source language.
//
// Using a typed constant (not a raw string) means:
//   - typos are caught at compile time
//   - switch statements can be exhaustive
//   - the zero value (LanguageUnknown) is safe and meaningful
type Language uint8

const (
	// LanguageUnknown is the zero value — returned when the file extension
	// has no registered grammar. Treat as "do not parse".
	LanguageUnknown Language = iota

	// LanguageGo represents Go source files (.go).
	LanguageGo

	// LanguagePython represents Python source files (.py).
	LanguagePython

	// LanguageJavaScript represents JavaScript source files (.js, .jsx).
	LanguageJavaScript

	// LanguageTypeScript represents TypeScript source files (.ts).
	LanguageTypeScript

	// LanguageTSX represents TypeScript JSX source files (.tsx).
	LanguageTSX

	// LanguageRust represents Rust source files (.rs).
	LanguageRust

	// LanguageC represents C source files (.c, .h).
	LanguageC

	// LanguageCPP represents C++ source files (.cpp, .cc, .cxx, .hpp).
	LanguageCPP

	// LanguageJava represents Java source files (.java).
	LanguageJava

	// LanguageBash represents shell script files (.sh, .bash).
	LanguageBash

	// LanguageJSON represents JSON data files (.json).
	LanguageJSON

	// LanguageHTML represents HTML files (.html, .htm).
	LanguageHTML

	// LanguageCSS represents CSS stylesheet files (.css).
	LanguageCSS
)
