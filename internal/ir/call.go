package ir

// CallIR represents a single outbound call expression.
//
// Go examples:
//
//	fmt.Println("hello")  → Caller: "main.main", Target: "fmt.Println", Kind: "external"
//	s.Scan()              → Caller: "main.main", Target: "Scan",         Kind: "internal"
//	len(x)                → Caller: "main.main", Target: "len",          Kind: "builtin"
type CallIR struct {
	// Caller is the qualified name of the function making the call.
	// Format: "package.FuncName" or "package.ReceiverType.MethodName"
	Caller string

	// Target is the name of the function or method being called.
	// May be qualified ("fmt.Println") or unqualified ("Scan") depending
	// on how it appears in source.
	Target string

	// Kind classifies the call target.
	Kind CallKind
}

// CallKind classifies the target of a call expression.
type CallKind string

const (
	// CallKindBuiltin is a call to a Go builtin: len, cap, make, new, etc.
	CallKindBuiltin CallKind = "builtin"

	// CallKindExternal is a call to a symbol from another package.
	// Identified by a qualified name with a dot: pkg.Symbol
	CallKindExternal CallKind = "external"

	// CallKindInternal is a call to a symbol within the same package.
	CallKindInternal CallKind = "internal"

	// CallKindMethod is a method call on a receiver: receiver.Method()
	CallKindMethod CallKind = "method"
)
