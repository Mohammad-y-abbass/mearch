package graph

// edgeKindMatch reports whether kind is in the kinds filter slice.
func edgeKindMatch(kind EdgeKind, kinds []EdgeKind) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}
