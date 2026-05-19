package graph

// Stats holds graph size metrics.
type Stats struct {
	TotalNodes int
	TotalEdges int
	ByKind     map[NodeKind]int
}
