// Package watcher implements the filesystem watcher for Mearch.
//
// The watcher listens for file changes and updates the graph incrementally
// so the retrieval engine stays current without re-indexing the entire repo.
//
// Architecture position:
//
//	Filesystem events
//	    ↓
//	[Watcher]
//	    ↓ reparse changed file
//	    ↓ extract new IR
//	    ↓ remove stale nodes from graph
//	    ↓ add new nodes and edges
//	    ↓ rebuild retrieval index for affected nodes
//	    ↓
//	Updated Graph + Retrieval Engine
//
// The watcher is the only component that mutates the graph after initial
// indexing. All other components treat the graph as read-only.
//
// Setup — add to go.mod:
//
//	go get github.com/fsnotify/fsnotify@latest
package watcher

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/mohamamd-y-abbass/mearch/internal/extractor"
	"github.com/mohamamd-y-abbass/mearch/internal/graph"
	"github.com/mohamamd-y-abbass/mearch/internal/ir"
	"github.com/mohamamd-y-abbass/mearch/internal/parser"
	"github.com/mohamamd-y-abbass/mearch/internal/retrieval"
	"github.com/mohamamd-y-abbass/mearch/internal/scanner"
)

// Event is a file change event the watcher detected.
type Event struct {
	// Path is the absolute path of the changed file.
	Path string

	// Op is what happened to the file.
	Op Op
}

// Op classifies what happened to a file.
type Op string

const (
	OpCreate Op = "create" // file was created
	OpWrite  Op = "write"  // file was modified
	OpDelete Op = "delete" // file was deleted
	OpRename Op = "rename" // file was renamed (old path)
)

// UpdateResult describes what changed in the graph after a file event.
type UpdateResult struct {
	// Path is the file that triggered the update.
	Path string

	// Op is what happened to the file.
	Op Op

	// NodesAdded is how many new nodes were added to the graph.
	NodesAdded int

	// NodesRemoved is how many stale nodes were removed from the graph.
	NodesRemoved int

	// EdgesAdded is how many new edges were added.
	EdgesAdded int

	// Duration is how long the incremental update took.
	Duration time.Duration

	// Error is non-nil if the update failed.
	Error error
}

// OnUpdateFunc is called after each incremental graph update.
// Use it to notify the MCP server that the index has changed.
type OnUpdateFunc func(result UpdateResult)

// WatcherConfig configures the watcher.
type WatcherConfig struct {
	// RootDir is the project root to watch.
	RootDir string

	// DebounceDelay is how long to wait after the last event before
	// processing. Prevents thrashing when editors write files rapidly
	// (e.g. saving multiple files in quick succession).
	// Default: 300ms
	DebounceDelay time.Duration

	// OnUpdate is called after each incremental graph update.
	// Called from a goroutine — must be safe for concurrent use.
	OnUpdate OnUpdateFunc

	// Logger for watcher events. Uses log.Default() if nil.
	Logger *log.Logger
}

// Watcher watches a project directory for changes and updates the graph
// incrementally. It is the only component that mutates the graph after
// initial indexing.
//
// Usage:
//
//	w, err := watcher.New(config, graph, fileIRs, engine)
//	if err != nil { ... }
//	defer w.Close()
//	go w.Run(ctx)
type Watcher struct {
	config    WatcherConfig
	fsWatcher *fsnotify.Watcher
	sc        *scanner.Scanner

	// Graph state — all protected by mu
	mu      sync.RWMutex
	g       *graph.Graph
	fileIRs map[string]*ir.FileIR // path → current FileIR for that file
	engine  *retrieval.Engine

	// Parser and extractor reused across updates
	p      *parser.Parser
	router *extractor.ExtractorRouter

	// Graph builder for incremental updates
	builder *graph.Builder

	// Debounce: pending events waiting to be processed
	pendingMu sync.Mutex
	pending   map[string]Op // path → latest op

	logger *log.Logger
}

// New constructs a Watcher.
//
// g, fileIRs, and engine must already be initialized (i.e. the initial
// index must be built before starting the watcher).
//
// fileIRs is a slice of the FileIRs used to build g — the watcher needs
// them to track which nodes came from which file (for stale node removal).
func New(
	config WatcherConfig,
	g *graph.Graph,
	initialFileIRs []*ir.FileIR,
	engine *retrieval.Engine,
) (*Watcher, error) {
	if config.RootDir == "" {
		return nil, fmt.Errorf("watcher: RootDir is required")
	}

	if config.DebounceDelay == 0 {
		config.DebounceDelay = 300 * time.Millisecond
	}

	logger := config.Logger
	if logger == nil {
		logger = log.Default()
	}

	// Build a per-file IR map from the initial file IRs.
	fileIRMap := make(map[string]*ir.FileIR, len(initialFileIRs))
	for _, fileIR := range initialFileIRs {
		fileIRMap[fileIR.Path] = fileIR
	}

	// Create the fsnotify watcher.
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watcher: could not create fsnotify watcher: %w", err)
	}

	// Create the scanner for extension checking.
	sc, err := scanner.NewScanner(config.RootDir, scanner.ScanOptions{})
	if err != nil {
		fsw.Close()
		return nil, fmt.Errorf("watcher: could not create scanner: %w", err)
	}

	w := &Watcher{
		config:    config,
		fsWatcher: fsw,
		sc:        sc,
		g:         g,
		fileIRs:   fileIRMap,
		engine:    engine,
		p:         parser.NewParser(),
		router:    extractor.NewExtractorRouter(),
		builder:   graph.NewBuilder(),
		pending:   make(map[string]Op),
		logger:    logger,
	}

	// Watch the root directory recursively.
	if err := w.watchDirRecursive(config.RootDir); err != nil {
		fsw.Close()
		return nil, fmt.Errorf("watcher: could not watch directory: %w", err)
	}

	return w, nil
}

// Run starts the watcher event loop. Blocks until ctx is cancelled.
// Call this in a goroutine:
//
//	go w.Run(ctx)
func (w *Watcher) Run(ctx context.Context) {
	w.logger.Printf("watcher: watching %s", w.config.RootDir)

	// Debounce timer — fires after DebounceDelay of inactivity.
	var debounceTimer *time.Timer
	debounceC := make(chan struct{}, 1)

	resetDebounce := func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
		}
		debounceTimer = time.AfterFunc(w.config.DebounceDelay, func() {
			select {
			case debounceC <- struct{}{}:
			default:
			}
		})
	}

	for {
		select {
		case <-ctx.Done():
			w.logger.Printf("watcher: shutting down")
			return

		case event, ok := <-w.fsWatcher.Events:
			if !ok {
				return
			}

			path := filepath.Clean(event.Name)

			// Only care about supported source files.
			if !w.sc.IsSupported(path) {
				// If a new directory was created, watch it.
				if event.Has(fsnotify.Create) {
					if info, err := os.Stat(path); err == nil && info.IsDir() {
						_ = w.watchDirRecursive(path)
					}
				}
				continue
			}

			// Map fsnotify op to our Op type.
			op := fsnotifyOpToOp(event.Op)
			if op == "" {
				continue
			}

			// Record pending event — last op wins for the same file.
			w.pendingMu.Lock()
			w.pending[path] = op
			w.pendingMu.Unlock()

			// Reset debounce timer.
			resetDebounce()

		case err, ok := <-w.fsWatcher.Errors:
			if !ok {
				return
			}
			w.logger.Printf("watcher: fsnotify error: %v", err)

		case <-debounceC:
			// Debounce fired — process all pending events.
			w.pendingMu.Lock()
			batch := make(map[string]Op, len(w.pending))
			for k, v := range w.pending {
				batch[k] = v
			}
			w.pending = make(map[string]Op)
			w.pendingMu.Unlock()

			for path, op := range batch {
				result := w.processEvent(ctx, path, op)
				if result.Error != nil {
					w.logger.Printf("watcher: update failed for %s: %v", path, result.Error)
				} else {
					w.logger.Printf(
						"watcher: updated %s [%s] +%d nodes -%d nodes +%d edges in %dms",
						filepath.Base(path),
						result.Op,
						result.NodesAdded,
						result.NodesRemoved,
						result.EdgesAdded,
						result.Duration.Milliseconds(),
					)
				}

				if w.config.OnUpdate != nil {
					w.config.OnUpdate(result)
				}
			}
		}
	}
}

// Close stops the watcher and releases resources.
func (w *Watcher) Close() {
	w.fsWatcher.Close()
	w.p.Close()
}

// Engine returns the current retrieval engine.
// The engine is rebuilt after each incremental update.
// Safe for concurrent use — protected by RWMutex.
func (w *Watcher) Engine() *retrieval.Engine {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.engine
}

// Graph returns the current graph.
// Safe for concurrent use — protected by RWMutex.
func (w *Watcher) Graph() *graph.Graph {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.g
}

// FileIRs returns a snapshot of all current FileIRs.
func (w *Watcher) FileIRs() []*ir.FileIR {
	w.mu.RLock()
	defer w.mu.RUnlock()
	result := make([]*ir.FileIR, 0, len(w.fileIRs))
	for _, fileIR := range w.fileIRs {
		result = append(result, fileIR)
	}
	return result
}

// =========================================================
// Incremental update pipeline
// =========================================================

// processEvent handles a single file change event.
//
// Pipeline:
//  1. If delete/rename: remove stale nodes from graph
//  2. If create/write: reparse file → extract IR → patch graph
//  3. Rebuild retrieval engine with updated graph + IRs
func (w *Watcher) processEvent(ctx context.Context, path string, op Op) UpdateResult {
	start := time.Now()

	result := UpdateResult{
		Path: path,
		Op:   op,
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	switch op {
	case OpDelete, OpRename:
		// File was deleted or renamed away — remove its nodes from the graph.
		removed := w.removeFileFromGraph(path)
		result.NodesRemoved = removed
		delete(w.fileIRs, path)

	case OpCreate, OpWrite:
		// File was created or modified.
		// Step 1: reparse the file first.
		// Keep existing graph state intact until we know replacement IR is valid.
		parseResult, err := w.p.ParseFile(ctx, path)
		if err != nil {
			result.Error = fmt.Errorf("reparse failed: %w", err)
			result.Duration = time.Since(start)
			return result
		}
		defer parseResult.Close()

		// Step 2: extract new IR.
		fileIR, err := w.router.Extract(parseResult)
		if err != nil {
			result.Error = fmt.Errorf("IR extraction failed: %w", err)
			result.Duration = time.Since(start)
			return result
		}

		// Step 3: remove stale nodes from the previous version of this file.
		if _, exists := w.fileIRs[path]; exists {
			removed := w.removeFileFromGraph(path)
			result.NodesRemoved = removed
		}

		// Step 4: count graph state before patching.
		statsBefore := w.g.Stats()

		// Step 5: patch the graph with new nodes and edges.
		w.builder.BuildOne(w.g, fileIR)

		// Step 6: store new IR.
		w.fileIRs[path] = fileIR

		// Step 7: count what changed.
		statsAfter := w.g.Stats()
		result.NodesAdded = statsAfter.TotalNodes - statsBefore.TotalNodes
		result.EdgesAdded = statsAfter.TotalEdges - statsBefore.TotalEdges
	}

	// Step 8: rebuild the retrieval engine.
	// This re-computes BM25, PageRank, co-occurrence for the updated graph.
	// It's the most expensive step — takes 100-500ms on typical repos.
	//
	// TODO: For Phase 4, make this incremental — only recompute the
	// affected portions of the index rather than the entire thing.
	allFileIRs := make([]*ir.FileIR, 0, len(w.fileIRs))
	for _, fileIR := range w.fileIRs {
		allFileIRs = append(allFileIRs, fileIR)
	}
	w.engine = retrieval.Build(w.g, allFileIRs, retrieval.DefaultTokenBudget)

	result.Duration = time.Since(start)
	return result
}

// removeFileFromGraph removes all nodes that originated from path.
//
// This works by collecting all node IDs in the graph whose FilePath
// matches path and removing them along with their edges.
//
// This is the correct approach for incremental updates — when a file
// changes, we remove everything it contributed and re-add the new version.
//
// Returns the number of nodes removed.
func (w *Watcher) removeFileFromGraph(path string) int {
	// Collect node IDs to remove.
	// We use the stored FileIR for this file — it records exactly which
	// qualified names were extracted from it.
	fileIR, ok := w.fileIRs[path]
	if !ok {
		return 0
	}

	nodeIDs := collectFileNodeIDs(fileIR)
	for _, id := range nodeIDs {
		w.g.RemoveNode(id)
	}

	return len(nodeIDs)
}

// collectFileNodeIDs returns all node IDs that a FileIR contributed to the graph.
func collectFileNodeIDs(fileIR *ir.FileIR) []string {
	ids := make([]string, 0, len(fileIR.Functions)+len(fileIR.Symbols)+2)

	// File node itself.
	ids = append(ids, fileIR.Path)

	// Function and method nodes.
	for _, fn := range fileIR.Functions {
		ids = append(ids, fn.Qualified)
	}

	// Symbol nodes.
	for _, sym := range fileIR.Symbols {
		ids = append(ids, sym.Qualified)
	}

	// Note: we do NOT remove the package node — other files in the same
	// package still depend on it. We do NOT remove external import nodes
	// either — other files might still import the same package.

	return ids
}

// =========================================================
// Directory watching
// =========================================================

// watchDirRecursive adds a directory and all its subdirectories to the
// fsnotify watcher, skipping ignored directories.
func (w *Watcher) watchDirRecursive(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable directories
		}

		if !d.IsDir() {
			return nil
		}

		// Skip ignored directories — same rules as the scanner.
		if path != root && isIgnoredDir(d.Name()) {
			return filepath.SkipDir
		}

		if watchErr := w.fsWatcher.Add(path); watchErr != nil {
			w.logger.Printf("watcher: could not watch %s: %v", path, watchErr)
		}

		return nil
	})
}

// =========================================================
// Helpers
// =========================================================

// fsnotifyOpToOp maps fsnotify operation flags to our Op type.
// Returns empty string for operations we don't care about (Chmod).
func fsnotifyOpToOp(op fsnotify.Op) Op {
	switch {
	case op.Has(fsnotify.Create):
		return OpCreate
	case op.Has(fsnotify.Write):
		return OpWrite
	case op.Has(fsnotify.Remove):
		return OpDelete
	case op.Has(fsnotify.Rename):
		return OpRename
	default:
		return ""
	}
}

// ignoredDirNames mirrors the scanner's ignored directories.
// Kept in sync manually — if you add to scanner.ignoredDirs, add here too.
var ignoredDirNames = map[string]bool{
	"node_modules": true, ".git": true, ".next": true, ".nuxt": true,
	".svelte-kit": true, ".turbo": true, "dist": true, "build": true,
	"out": true, "coverage": true, "__pycache__": true, ".cache": true,
	".hg": true, ".svn": true, "vendor": true, "target": true,
	".idea": true, ".vscode": true, "testdata": true, "fixtures": true,
}

func isIgnoredDir(name string) bool {
	return ignoredDirNames[name]
}

// =========================================================
// graph.Graph.RemoveNode — needs to be added to graph.go
// =========================================================
//
// The watcher requires RemoveNode on the graph. This is declared here
// as a note — it must be implemented in graph.go.
//
// Add this method to graph.go:
//
//	// RemoveNode removes a node and all its edges from the graph.
//	// Used by the watcher when a file is deleted or modified.
//	// Safe to call if the node does not exist (no-op).
//	func (g *Graph) RemoveNode(id string) {
//	    g.mu.Lock()
//	    defer g.mu.Unlock()
//
//	    if _, ok := g.nodes[id]; !ok {
//	        return
//	    }
//
//	    // Remove all outgoing edges from this node.
//	    for _, edge := range g.out[id] {
//	        // Remove this node from the incoming index of the target.
//	        g.in[edge.To] = removeEdgeFrom(g.in[edge.To], id)
//	    }
//	    delete(g.out, id)
//
//	    // Remove all incoming edges to this node.
//	    for _, edge := range g.in[id] {
//	        // Remove this node from the outgoing index of the source.
//	        g.out[edge.From] = removeEdgeFrom(g.out[edge.From], id)
//	    }
//	    delete(g.in, id)
//
//	    // Remove the node itself.
//	    delete(g.nodes, id)
//	}
//
//	// removeEdgeFrom removes all edges in the slice that point to/from nodeID.
//	func removeEdgeFrom(edges []Edge, nodeID string) []Edge {
//	    result := edges[:0]
//	    for _, e := range edges {
//	        if e.From != nodeID && e.To != nodeID {
//	            result = append(result, e)
//	        }
//	    }
//	    return result
//	}

// =========================================================
// WatcherStatus — for MCP server status reporting
// =========================================================

// Status returns a human-readable status string for the watcher.
func (w *Watcher) Status() string {
	w.mu.RLock()
	defer w.mu.RUnlock()

	stats := w.g.Stats()
	return fmt.Sprintf(
		"watching %s | %d files | %d nodes | %d edges",
		w.config.RootDir,
		len(w.fileIRs),
		stats.TotalNodes,
		stats.TotalEdges,
	)
}

// keep strings import used in path handling
var _ = strings.TrimPrefix
