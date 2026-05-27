package parser

import "fmt"

// ErrUnsupportedLanguage is returned when a file's extension has no
// corresponding Tree-sitter grammar registered in this package.
type ErrUnsupportedLanguage struct {
	Path string
	Ext  string
}

func (e *ErrUnsupportedLanguage) Error() string {
	if e.Ext != "" {
		return fmt.Sprintf("parser: unsupported language (ext=%q, path=%s)", e.Ext, e.Path)
	}
	return fmt.Sprintf("parser: unsupported language (path=%s)", e.Path)
}
