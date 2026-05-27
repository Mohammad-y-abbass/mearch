# Mearch

**Mearch** is a code-intelligence engine for AI coding agents. It indexes a project into a semantic graph, then answers natural-language questions with ranked symbols, files, and relationships—within a token budget—so agents spend less time exploring large repositories.

It ships as a [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) server over stdio, so tools like **Cursor**, **Claude Desktop**, and other MCP clients can call it directly.

## Features

- **Project scanning** — Walks the workspace while skipping common noise (`node_modules`, `.git`, `vendor`, build outputs, etc.).
- **Tree-sitter parsing** — Syntax-aware parsing for multiple languages.
- **IR extraction** — Pulls imports, functions, types, symbols, and call sites into a unified intermediate representation.
- **Semantic code graph** — Directed graph of packages, files, symbols, and edges (imports, calls, containment, etc.).
- **Hybrid retrieval** — BM25 + graph signals (PageRank, betweenness, caller counts) + co-occurrence expansion + graph traversal, compressed to a configurable token budget (default **4000** tokens).
- **MCP tools** — `index_project`, `query_context`, `find_symbol`, `find_callers`, `trace_deps`, `graph_stats`.
- **Auto-index on connect** — When the IDE connects, Mearch indexes the workspace root from MCP roots (or `MEARCH_PROJECT_PATH`).

## Supported languages

| Language   | Extensions (examples)              | Indexed (IR + graph) |
|-----------|-------------------------------------|----------------------|
| Go        | `.go`                               | Yes                  |
| Python    | `.py`                               | Yes                  |
| JavaScript| `.js`, `.jsx`, `.mjs`, `.cjs`       | Yes                  |
| TypeScript| `.ts`                               | Yes                  |
| TSX       | `.tsx`                              | Yes                  |
| Rust      | `.rs`                               | Yes                  |
| Java      | `.java`                             | Yes                  |
| C / C++   | `.c`, `.h`, `.cpp`, `.hpp`, …      | Yes                  |

The parser also recognizes **Bash**, **JSON**, **HTML**, and **CSS** by extension, but only languages with a registered extractor are fully indexed into the graph. Language detection is **extension-based** via `parser.LanguageForFile()` in `internal/parser/utils.go`.

## Architecture

```
Scanner → Parser (Tree-sitter) → Extractor (per-language IR)
    → Graph builder → Retrieval engine (BM25 + graph traversal)
        → MCP tools
```

| Package            | Role |
|--------------------|------|
| `internal/scanner` | Discover source files; respect ignore rules and depth limits |
| `internal/parser`  | Parse files; map extensions → `Language` |
| `internal/extractor` | Language-specific IR from syntax trees |
| `internal/ir`      | Shared IR types (files, functions, imports, calls) |
| `internal/graph`   | In-memory directed code graph |
| `internal/retrieval` | Index-time scoring + query-time pipeline |
| `internal/mcp`     | MCP server, tools, auto-index |
| `cmd/mearch`       | Server entrypoint |
| `cmd/benchmark`    | Benchmark harness vs. agent trajectories |

## Requirements

- **Go 1.25+** (see `go.mod`)
- An MCP client (e.g. Cursor) for normal use
- **Optional (benchmarks):** Python 3 with `datasets`, `tiktoken`, `gitpython` for `trajectory_extractor.py`

## Quick start

### 1. Build

```bash
go build -o mearch ./cmd/mearch
```

On Windows the binary is typically `mearch.exe`.

### 2. Configure MCP (Cursor example)

Add to your MCP settings (e.g. `.cursor/mcp.json` or global MCP config):

```json
{
  "mcpServers": {
    "mearch": {
      "command": "C:\\path\\to\\mearch.exe",
      "args": []
    }
  }
}
```

Use the absolute path to your built binary. On macOS/Linux:

```json
{
  "mcpServers": {
    "mearch": {
      "command": "/usr/local/bin/mearch",
      "args": []
    }
  }
}
```

Or run without installing:

```json
{
  "mcpServers": {
    "mearch": {
      "command": "go",
      "args": ["run", "./cmd/mearch"],
      "cwd": "C:\\Users\\You\\Desktop\\mearch"
    }
  }
}
```

Restart the MCP client after changing config. Mearch logs to **stderr**; MCP uses **stdio** for the protocol.

### 3. Optional: force project path

If auto-index cannot resolve the workspace root, set:

```bash
export MEARCH_PROJECT_PATH=/absolute/path/to/your/project
```

(Windows: `set MEARCH_PROJECT_PATH=...`)

## MCP tools

### `index_project`

Scans and indexes a directory. Also runs automatically when the client connects (unless the same root is already indexed).

| Argument     | Description |
|-------------|-------------|
| `path`      | Project root (required) |
| `max_depth` | Max directory depth (default `10`; `0` is treated as `10`) |

### `query_context`

Primary retrieval tool: natural-language query → ranked symbols and files within the token budget.

| Argument       | Description |
|----------------|-------------|
| `query`        | Natural language question (required) |
| `token_budget` | Max output tokens (default `4000`) |
| `debug`        | Include scoring details |

Example queries: `"scanner ignore rules"`, `"how does the parser handle errors"`, `"graph traversal logic"`.

### `find_symbol`

Look up symbols by name (partial match). Optional filters: `kind`, `package`.

### `find_callers`

List callers of a function or method (e.g. `"Scanner.Scan"`).

### `trace_deps`

Follow outgoing dependencies from a symbol (default depth `2`, max `5`).

### `graph_stats`

Node/edge counts and breakdown by node kind for the current index.

## Development

### Run tests

```bash
go test ./...
```

### Run the server locally

```bash
go run ./cmd/mearch
```

The process expects an MCP client on stdio; running alone will appear idle until a client connects.

### Project layout

```
cmd/
  mearch/          # MCP server main
  benchmark/       # Retrieval benchmark CLI
internal/
  mcp/             # MCP server & tools
  scanner/         # File discovery
  parser/          # Tree-sitter parsing
  extractor/       # Per-language IR
  ir/              # Intermediate representation
  graph/           # Code graph
  retrieval/       # BM25 + graph retrieval
benchmark_data/    # Sample tasks.json for benchmarks
trajectory_extractor.py
```

## Benchmarking

Mearch can be compared against real agent “exploration” cost using SWE-bench-style trajectories.

### 1. Extract tasks (Python)

```bash
pip install datasets tiktoken gitpython
python trajectory_extractor.py --output-dir ./benchmark_data --max-tasks 20
```

This writes `benchmark_data/tasks.json` and may clone repos under `benchmark_data/repos/` (gitignored).

### 2. Run benchmark (Go)

```bash
go run ./cmd/benchmark --tasks ./benchmark_data/tasks.json
go run ./cmd/benchmark --tasks ./benchmark_data/tasks.json --debug
go run ./cmd/benchmark --tasks ./benchmark_data/tasks.json --output benchmark_results.json
```

Results compare Mearch retrieval token usage and relevance against baseline agent tool-call overhead from the trajectories.

## How retrieval works (summary)

At **index time**, Mearch builds enriched documents, a BM25 index, a co-occurrence matrix, and graph-based scores (PageRank, betweenness, caller counts).

At **query time**, the pipeline:

1. Preprocesses and expands the query (synonyms / co-occurrence / graph neighbors)
2. Detects **seed** nodes (BM25, comments, fuzzy match, intent hints)
3. Traverses the graph bidirectionally from seeds
4. Fuses lexical and graph scores
5. Compresses results to fit the token budget (including “read whole file” hints for high-relevance files)

See `internal/retrieval/retrieval.go` for the full pipeline.

## Adding a language

1. Add a `Language` constant in `internal/parser/languages.go`.
2. Map extensions in `internal/parser/utils.go` (`languageForExt`).
3. Register the Tree-sitter grammar in `internal/parser/parser.go` (`treeSitterLanguage`).
4. Add the extension to `internal/scanner/extensions.go` for discovery.
5. Implement an extractor in `internal/extractor/` and register it in `internal/extractor/extractor.go`.

Parser and scanner extension lists should stay in sync.

## Troubleshooting

| Issue | What to check |
|-------|----------------|
| `project not indexed` | Wait for auto-index after connect, or call `index_project` with an absolute path |
| Many files skipped | Language may parse but lack an extractor, or extension is unsupported |
| Empty `query_context` results | Try a more specific query; run `graph_stats` to confirm the index size |
| Wrong project indexed | Set `MEARCH_PROJECT_PATH` or call `index_project` with the correct `path` |
| MCP not starting | Verify binary path in config; check stderr logs from the MCP process |

## License

See repository license file if present; otherwise check with the maintainer.
