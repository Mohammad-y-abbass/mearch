package ir

// FunctionIR represents a function or method declaration.
//
// Go examples:
//
//	func Foo() {}                       → Name: "Foo",  Receiver: ""
//	func (s *Scanner) Scan() {}        → Name: "Scan", Receiver: "*Scanner"
type FunctionIR struct {
	// Name is the unqualified function or method name.
	Name string

	// Qualified is the fully qualified name including package and receiver.
	// Used as a stable, collision-free identifier in the graph.
	//
	// Format for functions: "package.FuncName"
	// Format for methods:   "package.ReceiverType.MethodName"
	//
	// Example: "scanner.Scanner.Scan"
	Qualified string

	// Receiver is the method receiver type, if this is a method.
	// Empty for plain functions.
	// Stored without pointer indicator for normalisation:
	// both "(s *Scanner)" and "(s Scanner)" produce Receiver: "Scanner".
	Receiver string

	// Visibility indicates whether the symbol is exported.
	// "exported"   — name starts with uppercase
	// "unexported" — name starts with lowercase
	Visibility string
}
