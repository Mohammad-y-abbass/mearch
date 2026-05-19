package ir

// EdgeIR represents an explicit semantic relationship between two entities.
//
// Edges are the raw material for the graph builder.
// Every relationship in the codebase that Mearch cares about is
// eventually expressed as one or more EdgeIRs.
//
// Examples:
//
//	main.go imports fmt          → From: "main.go",      To: "fmt",         Kind: EdgeKindImport
//	Scanner defines Scan         → From: "Scanner",      To: "Scan",        Kind: EdgeKindDefine
//	main calls fmt.Println       → From: "main.main",    To: "fmt.Println", Kind: EdgeKindCall
type EdgeIR struct {
	// From is the qualified name of the source entity.
	From string

	// To is the qualified name of the target entity.
	To string

	// Kind describes the semantic relationship.
	Kind EdgeKind
}

// EdgeKind describes the type of relationship an EdgeIR represents.
type EdgeKind string

const (
	// EdgeKindImport: From file imports To package.
	EdgeKindImport EdgeKind = "import"

	// EdgeKindCall: From function calls To function.
	EdgeKindCall EdgeKind = "call"

	// EdgeKindDefine: From type defines To method or field.
	EdgeKindDefine EdgeKind = "define"

	// EdgeKindUse: From symbol uses To type.
	EdgeKindUse EdgeKind = "use"

	// EdgeKindInherit: From type inherits/embeds To type.
	EdgeKindInherit EdgeKind = "inherit"

	// EdgeKindImplement: From type implements To interface.
	EdgeKindImplement EdgeKind = "implement"

	// EdgeKindCompose: From struct composes To struct via embedding.
	EdgeKindCompose EdgeKind = "compose"
)
