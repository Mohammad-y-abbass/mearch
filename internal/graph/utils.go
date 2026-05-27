package graph

import "slices"

// edgeKindMatch reports whether kind is in the kinds filter slice.
func edgeKindMatch(kind EdgeKind, kinds []EdgeKind) bool {
	return slices.Contains(kinds, kind)
}
