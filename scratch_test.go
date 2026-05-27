//go:build ignore

package main

import (
	"fmt"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_bash "github.com/tree-sitter/tree-sitter-bash/bindings/go"
	tree_sitter_c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tree_sitter_cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tree_sitter_css "github.com/tree-sitter/tree-sitter-css/bindings/go"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tree_sitter_html "github.com/tree-sitter/tree-sitter-html/bindings/go"
	tree_sitter_java "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tree_sitter_javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tree_sitter_json "github.com/tree-sitter/tree-sitter-json/bindings/go"
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tree_sitter_rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

func main() {
	_ = tree_sitter.NewLanguage(tree_sitter_go.Language())
	_ = tree_sitter.NewLanguage(tree_sitter_javascript.Language())
	_ = tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTypescript())
	_ = tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTSX())
	_ = tree_sitter.NewLanguage(tree_sitter_python.Language())
	_ = tree_sitter.NewLanguage(tree_sitter_rust.Language())
	_ = tree_sitter.NewLanguage(tree_sitter_c.Language())
	_ = tree_sitter.NewLanguage(tree_sitter_cpp.Language())
	_ = tree_sitter.NewLanguage(tree_sitter_java.Language())
	_ = tree_sitter.NewLanguage(tree_sitter_bash.Language())
	_ = tree_sitter.NewLanguage(tree_sitter_json.Language())
	_ = tree_sitter.NewLanguage(tree_sitter_css.Language())
	_ = tree_sitter.NewLanguage(tree_sitter_html.Language())
	fmt.Println("Success!")
}
