# Mearch

> **Graph-native code intelligence for AI coding agents.**

Mearch is a local-first code intelligence engine that transforms your codebase from raw text into a structured semantic graph, then exposes intelligent retrieval APIs that give AI agents exactly the context they need.

---

## The Problem

Modern AI coding agents are powerful but wasteful. Without structural understanding of a codebase, they resort to:

- Reading entire files when they only need one function
- Making 4–8 tool calls to find context that should take 1
- Consuming thousands of tokens on exploration before writing a single line of code
- Missing relationships between symbols that are not co-located in the same file

On a large repository, this makes AI assistance slow, expensive, and imprecise. The root cause is that every existing tool treats a codebase as **text** — a flat collection of files to search through.

**Mearch treats a codebase as a graph.**

---

## How It Works

```
Source Code
    ↓
Scanner          discovers eligible files across the repo
    ↓
Parser           produces syntax trees via Tree-sitter (11 languages)
    ↓
IR Extractor     converts syntax trees into language-agnostic semantic IR
    ↓
Graph Builder    builds a directed graph of nodes and edges
    ↓
Retrieval Engine ranks relevant context using BM25 + graph traversal + PageRank
    ↓
MCP Server       exposes everything to AI agents via the Model Context Protocol
```

When an AI agent needs context for a task, it calls `query_context("scanner ignore rules")` and receives the most relevant symbols, functions, and files — compressed to fit a token budget — in a single round trip.

---

## Key Features

### Graph-Native Retrieval

Every symbol in the codebase becomes a node. Every relationship — imports, calls, defines, implements — becomes an edge. Retrieval follows these edges bidirectionally, surfacing not just the queried symbol but everything that calls it and everything it depends on.

### Multi-Strategy Seed Detection

Five strategies run in parallel to find the best starting nodes for any query:

- **BM25 name search**: exact and stemmed matches against symbol names
- **BM25 comment search**: matches against docstrings and comments
- **BM25 expanded search**: matches after query expansion via co-occurrence and synonyms
- **Fuzzy matching**: Levenshtein edit distance for typos and near-matches
- **Structural intent**: detects query patterns like "find callers of X" and adjusts traversal direction accordingly

### Field-Weighted BM25

Documents are not just symbol names. Each symbol's BM25 document contains:

```
name          weight 3.0   symbol name tokens
qualified     weight 2.5   full qualified name
comment       weight 2.0   docstrings and comments
callers       weight 1.5   who calls this symbol
callees       weight 1.5   what this symbol calls
siblings      weight 1.2   other methods on same receiver
filepath      weight 1.0   file path segments
imports       weight 0.5   imported packages
```

This means a query for `"directory traversal"` finds `Scanner.Scan` through its comment even though neither word appears in the function name.

### Query Expansion

Before searching, queries are expanded using:

- **Co-occurrence matrix**: terms that always appear near query terms in this specific codebase
- **Programming synonym table**: hand-curated synonyms for common code vocabulary (`"scan"` → `"walk"`, `"traverse"`, `"discover"`)
- **Graph neighbor injection**: tokens from graph-adjacent nodes are injected into the query

### PageRank Centrality

PageRank is computed across the entire graph at index time. Load-bearing symbols that everything depends on score higher. External stdlib packages are discounted by 90% — they have high raw centrality but are irrelevant for code navigation.

### Bidirectional Weighted BFS

Traversal from seed nodes follows both directions:

- **Forward**: what does this symbol depend on?
- **Backward**: who uses this symbol? ← often the most valuable direction

Edges are weighted by semantic tightness (`define=0.90`, `call=0.80`, `import=0.45`). A priority queue ensures high-weight paths fill the candidate budget before low-weight ones.

### Tiered Token Compression

Results are compressed to a configurable token budget in three tiers:

1. **Always include**: seed nodes, direct callers, exact name matches
2. **Fill to 60% budget**: score ≥ 0.50, exported symbols, depth ≤ 2
3. **Fill to 90% budget**: score ≥ 0.30, depth ≤ 3

Files with 4+ relevant symbols are collapsed into a single file-level entry, saving significant tokens while telling the agent exactly which file to read.

### Local-First, Zero Infrastructure

- No hosted services
- No cloud dependencies
- No remote indexing pipelines
- No API keys required
- Runs entirely on developer machines or CI

---

## Supported Languages

| Language   | Extension(s)                          | Parser                 |
| ---------- | ------------------------------------- | ---------------------- |
| Go         | `.go`                                 | tree-sitter-go         |
| TypeScript | `.ts`, `.mts`                         | tree-sitter-typescript |
| TSX        | `.tsx`                                | tree-sitter-typescript |
| JavaScript | `.js`, `.mjs`, `.cjs`                 | tree-sitter-javascript |
| JSX        | `.jsx`                                | tree-sitter-javascript |
| Python     | `.py`, `.pyw`                         | tree-sitter-python     |
| Rust       | `.rs`                                 | tree-sitter-rust       |
| Java       | `.java`                               | tree-sitter-java       |
| Ruby       | `.rb`                                 | tree-sitter-ruby       |
| C          | `.c`, `.h`                            | tree-sitter-c          |
| C++        | `.cpp`, `.cc`, `.cxx`, `.hpp`, `.hxx` | tree-sitter-cpp        |

---

## Installation

### Prerequisites

- Go 1.25+
- Git

### Build from source

```bash
git clone https://github.com/Mohammad-y-abbass/mearch
cd mearch
go build ./cmd/mearch
```

## MCP Server

Mearch exposes its retrieval engine via the Model Context Protocol so any MCP-compatible AI agent can use it.

### How the graph is built

When the MCP server starts, it does not immediately index anything. Indexing is triggered automatically when an IDE session begins — specifically, when the MCP client sends its first initialization message. At that point, `index_project` is called automatically on the workspace root, building the full semantic graph before any queries arrive. This means the graph is always ready by the time the agent needs context.

If you want to explicitly re-index (after major code changes), call `index_project` manually.

### Configuration

Add Mearch to your MCP client configuration:

**Claude Desktop** (`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "mearch": {
      "command": "/path/to/mearch",
      "args": ["serve"],
      "env": {
        "MEARCH_ROOT": "/path/to/your/project"
      }
    }
  }
}
```

**Cursor** (`.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "mearch": {
      "command": "/path/to/mearch",
      "args": ["serve"]
    }
  }
}
```

**Windsurf** (`~/.codeium/windsurf/mcp_config.json`):

```json
{
  "mcpServers": {
    "mearch": {
      "command": "/path/to/mearch",
      "args": ["serve"]
    }
  }
}
```

### Available MCP Tools

#### `index_project`

Scans and indexes a project directory. Called automatically on IDE load.

```
Arguments:
  path        string   Absolute or relative path to the project root
  max_depth   int      Maximum directory depth to scan (default: 10)
```

#### `query_context`

Retrieves the most relevant code context for a natural language query. This is the primary tool — use it before making any code changes.

```
Arguments:
  query        string   Natural language description of what you're looking for
  token_budget int      Maximum tokens to return (default: 4000)
  debug        bool     Include scoring details for each result

Returns:
  Ranked list of relevant symbols with file locations, scores, and relationships
```

Example queries:

```
"scanner ignore rules"
"how does the parser handle errors"
"find the graph traversal logic"
"authentication and session management"
"database connection pooling"
```

#### `find_symbol`

Finds a specific symbol by name with partial matching.

```
Arguments:
  name     string   Symbol name (partial match supported)
  kind     string   Filter by: function method struct interface const var file package
  package  string   Filter by package name
```

#### `find_callers`

Finds all code that calls a specific function or method. Use this before changing a function signature.

```
Arguments:
  symbol   string   Fully or partially qualified symbol name
```

#### `trace_deps`

Traces the dependency chain of a symbol up to a specified depth.

```
Arguments:
  symbol   string   Symbol to trace
  depth    int      How many hops to follow (default: 2, max: 5)
```

#### `graph_stats`

Returns statistics about the indexed project graph. Useful for verifying the index is complete.

---

## CLI

```bash
# Start MCP server (primary usage)
mearch serve

# Index a project and print stats
mearch index ./myproject

# Query context from the command line
mearch query "scanner ignore rules"

# Query with custom token budget
mearch query "authentication flow" --tokens 2000

# Show graph statistics
mearch stats ./myproject

# Watch a project for changes and keep index live
mearch watch ./myproject
```

---

## Project Structure

---

## Architecture Deep Dive

### Why graph-native retrieval

The central insight of Mearch is that codebases are graphs, not text blobs. Consider this query:

```
"fix login button loading state"
```

A naive system searches for files containing "login", "button", "loading". It finds `LoginButton.tsx` directly.

Mearch does more. It finds `LoginButton.tsx` as a seed, then traverses the graph:

```
LoginButton.tsx                           ← seed (depth 0)
    ↑ called by: App.tsx                  ← backward depth 1
    ↓ calls: useAuth hook                 ← forward depth 1
    ↓ calls: useLoadingState hook         ← forward depth 1
    ↓ imports: Button component           ← forward depth 1
        ↓ imports: styles/button.css      ← forward depth 2
```

The agent gets `LoginButton.tsx`, `useAuth.ts`, `useLoadingState.ts`, `Button.tsx`, and `button.css` — exactly what it needs to fix the loading state — in one call, without exploring the filesystem.

### Why no AI models

Mearch achieves near-semantic retrieval quality without any machine learning models:

- **Query expansion** from the codebase's own co-occurrence patterns acts as a learned vocabulary
- **Field-weighted BM25** on enriched documents (including comments and callers) closes the vocabulary gap
- **Graph traversal** surfaces structurally related symbols that text search cannot find
- **PageRank centrality** identifies load-bearing symbols without any training

This means Mearch runs identically on a MacBook Pro M3 and a cheap CI server. No GPU, no model downloads, no inference latency.

---
### What is measured

| Metric                 | Description                                                   |
| ---------------------- | ------------------------------------------------------------- |
| **Baseline tokens**    | Tokens in edited files read in full (minimum any agent needs) |
| **Mearch tokens**      | Tokens returned by `query_context`                            |
| **Token savings %**    | `(baseline - mearch) / baseline × 100`                        |
| **Round trip savings** | Agent tool calls before: 4–8. With Mearch: 1                  |
| **File recall**        | Did Mearch return the files the agent needed?                 |
| **Symbol recall**      | Did Mearch return the symbols the agent edited?               |

---

## Adding a New Language

Mearch is designed so that adding a language requires minimal changes:

**1. Add the extension to `scanner.go`:**

```go
var supportedExtensions = map[string]string{
    ".kt": "kotlin",  // ← add this
}
```

**2. Add the language constant and grammar to `parser.go`:**

```go
const (
    LanguageKotlin Language = iota + 12  // ← add constant
)

var languageForExt = map[string]Language{
    ".kt": LanguageKotlin,  // ← add mapping
}

// In treeSitterLanguage():
case LanguageKotlin:
    return tree_sitter.NewLanguage(tree_sitter_kotlin.Language()), nil
```

**3. Create `extractor_kotlin.go`:**

```go
type KotlinExtractor struct { tsLang *tree_sitter.Language }
func NewKotlinExtractor() *KotlinExtractor { ... }
func (e *KotlinExtractor) Extract(result *parser.ParseResult) (*ir.FileIR, error) { ... }
```

**4. Register in `extractor.go`:**

```go
r.extractors[parser.LanguageKotlin] = NewKotlinExtractor()
```

Everything else — graph builder, retrieval engine, MCP server, benchmarks — works immediately with no changes.

---

## Future Goals

The following features are planned for future releases, in priority order:

#### File Watcher

Use `fsnotify` to watch for filesystem changes and update the graph incrementally as files are edited. The graph stays live without manual re-indexing.

## Contributing

### Code style

- Go standard formatting (`gofmt`)
- Comments on every exported symbol
- Every design decision has a comment explaining why, not just what

### Filing issues

Please include:

- The query that produced unexpected results
- The repo and file structure (anonymized if needed)
- The actual vs expected results from `query_context` with `debug: true`

---

## License

MIT License — see LICENSE file.

---

## Philosophy

Mearch is not:

- An LLM wrapper
- A vector database
- A chatbot framework
- A generic AI SDK

Mearch is a graph-native code intelligence engine.

Its goal is to transform codebases from raw text into structured semantic systems for efficient AI-assisted development.

The long-term vision is not merely better retrieval. It is structural understanding of software systems — which becomes increasingly critical as repositories grow larger, AI agents become more autonomous, token efficiency becomes essential, and software systems become more interconnected.

> _Codebases are graphs, not text blobs._
