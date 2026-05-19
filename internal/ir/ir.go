// Package ir defines the Intermediate Representation types for Mearch.
//
// The IR is the semantic foundation of the entire engine.
// Everything above the parser layer operates on IR, never on raw ASTs.
//
// Core philosophy:
//
//	IR represents WHAT code means, not HOW it is written.
//
// A FileIR is the unit of indexing. One source file produces one FileIR.
// The graph builder consumes FileIRs and converts them into nodes and edges.
//
// IR types are intentionally simple value structs — no methods, no behaviour.
// They are pure data. Validation and construction logic lives in the extractor.
package ir
