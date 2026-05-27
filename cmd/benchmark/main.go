// benchmark/main.go
//
// Runs the Mearch benchmark against real agent trajectory data.
//
// Takes tasks.json produced by trajectory_extractor.py, indexes each repo
// with Mearch, runs the query, and compares token usage and quality against
// what the real agent did without Mearch.
//
// Usage:
//
//	go run ./cmd/benchmark --tasks ./benchmark_data/tasks.json
//	go run ./cmd/benchmark --tasks ./benchmark_data/tasks.json --debug
//	go run ./cmd/benchmark --tasks ./benchmark_data/tasks.json --output results.json
//
// Install tiktoken for accurate token counting:
//
//	go get github.com/pkoukk/tiktoken-go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mohamamd-y-abbass/mearch/internal/extractor"
	"github.com/mohamamd-y-abbass/mearch/internal/graph"
	"github.com/mohamamd-y-abbass/mearch/internal/ir"
	"github.com/mohamamd-y-abbass/mearch/internal/parser"
	"github.com/mohamamd-y-abbass/mearch/internal/retrieval"
	"github.com/mohamamd-y-abbass/mearch/internal/scanner"
	tiktoken "github.com/pkoukk/tiktoken-go"
)

// =========================================================
// Input types (from tasks.json produced by Python script)
// =========================================================

// TaskBaseline is the JSON structure produced by trajectory_extractor.py.
type TaskBaseline struct {
	InstanceID          string     `json:"instance_id"`
	Repo                string     `json:"repo"`
	BaseCommit          string     `json:"base_commit"`
	ProblemStatement    string     `json:"problem_statement"`
	RepoPath            string     `json:"repo_path"`
	ToolCalls           []ToolCall `json:"tool_calls"`
	ExploreTokens       int        `json:"explore_tokens"`
	ExploreCalls        int        `json:"explore_calls"`
	UnderstandTokens    int        `json:"understand_tokens"`
	UnderstandCalls     int        `json:"understand_calls"`
	EditedFiles         []string   `json:"edited_files"`
	EditedSymbols       []string   `json:"edited_symbols"`
	TotalOverheadTokens int        `json:"total_overhead_tokens"`
	TotalOverheadCalls  int        `json:"total_overhead_calls"`
	Patch               string     `json:"patch"`
}

type ToolCall struct {
	Step   int    `json:"step"`
	Tool   string `json:"tool"`
	Input  string `json:"input"`
	Output string `json:"output"`
	Tokens int    `json:"tokens"`
	Phase  string `json:"phase"`
}

// =========================================================
// Output types
// =========================================================

// BenchmarkResult holds all metrics for a single task.
type BenchmarkResult struct {
	// Identity
	InstanceID  string `json:"instance_id"`
	Repo        string `json:"repo"`
	Problem     string `json:"problem"`
	MearchQuery string `json:"mearch_query"`

	// Baseline — what the real agent did without Mearch
	BaselineExploreTokens    int `json:"baseline_explore_tokens"`
	BaselineExploreCalls     int `json:"baseline_explore_calls"`
	BaselineUnderstandTokens int `json:"baseline_understand_tokens"`
	BaselineUnderstandCalls  int `json:"baseline_understand_calls"`
	BaselineTotalTokens      int `json:"baseline_total_tokens"`
	BaselineTotalCalls       int `json:"baseline_total_calls"`

	// File-content baseline (for tasks without tool call data)
	// = tokens in the edited files read in full
	FileContentBaselineTokens int `json:"file_content_baseline_tokens"`

	// Mearch output
	MearchTokens      int   `json:"mearch_tokens"`
	MearchCalls       int   `json:"mearch_calls"` // always 1
	MearchLatencyMs   int64 `json:"mearch_latency_ms"`
	MearchResultCount int   `json:"mearch_result_count"`
	MearchSeedsCount  int   `json:"mearch_seeds_count"`
	MearchCandidates  int   `json:"mearch_candidates"`

	// Savings vs trajectory baseline
	SavedVsTrajectory    int     `json:"saved_vs_trajectory"`
	SavingsPctTrajectory float64 `json:"savings_pct_trajectory"`

	// Savings vs file-content baseline
	SavedVsFileContent    int     `json:"saved_vs_file_content"`
	SavingsPctFileContent float64 `json:"savings_pct_file_content"`

	// Explore waste eliminated (100% — Mearch never has an explore phase)
	ExploreWasteEliminated int     `json:"explore_waste_eliminated"`
	ExploreWastePct        float64 `json:"explore_waste_pct"`

	// Quality — did Mearch return what the agent needed?
	EditedFilesFound    []string `json:"edited_files_found"`
	EditedFilesMissed   []string `json:"edited_files_missed"`
	EditedSymbolsFound  []string `json:"edited_symbols_found"`
	EditedSymbolsMissed []string `json:"edited_symbols_missed"`
	FileRecall          float64  `json:"file_recall"`
	SymbolRecall        float64  `json:"symbol_recall"`
	OverallRecall       float64  `json:"overall_recall"`

	// Expanded query tokens (for debugging)
	ExpandedTokens []string `json:"expanded_tokens,omitempty"`

	// Whether this task had real trajectory data or just file-content baseline
	HasTrajectoryData bool `json:"has_trajectory_data"`

	// Error if any
	Error string `json:"error,omitempty"`
}

// =========================================================
// Main
// =========================================================

func main() {
	tasksFile := flag.String("tasks", "", "Path to tasks.json from trajectory_extractor.py (required)")
	outputFile := flag.String("output", "benchmark_results.json", "Output file for results")
	debug := flag.Bool("debug", false, "Print detailed output per task")
	maxTasks := flag.Int("max", 0, "Maximum tasks to run (0 = all)")
	flag.Parse()

	if *tasksFile == "" {
		fmt.Fprintln(os.Stderr, "usage: benchmark --tasks tasks.json [--output results.json] [--debug] [--max N]")
		os.Exit(1)
	}

	// Load tasks
	tasks, err := loadTasks(*tasksFile)
	if err != nil {
		log.Fatalf("failed to load tasks: %v", err)
	}
	if *maxTasks > 0 && len(tasks) > *maxTasks {
		tasks = tasks[:*maxTasks]
	}

	// Initialize tiktoken for accurate token counting
	enc, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		log.Fatalf("failed to init tokenizer: %v", err)
	}

	printHeader("MEARCH BENCHMARK")
	fmt.Printf("tasks file:  %s\n", *tasksFile)
	fmt.Printf("tasks count: %d\n", len(tasks))
	fmt.Println()

	// Run benchmarks
	results := make([]BenchmarkResult, 0, len(tasks))

	for i, task := range tasks {
		fmt.Printf("[%d/%d] %s\n", i+1, len(tasks), task.InstanceID)
		fmt.Printf("       repo:    %s\n", task.Repo)
		fmt.Printf("       problem: %s\n", truncate(task.ProblemStatement, 80))

		result := runTask(task, enc, *debug)
		results = append(results, result)

		if result.Error != "" {
			fmt.Printf("       ✗ error: %s\n\n", result.Error)
			continue
		}

		printTaskResult(result)
		fmt.Println()
	}

	// Print summary
	printSummary(results)

	// Save results
	if err := saveResults(results, *outputFile); err != nil {
		fmt.Printf("warning: could not save results: %v\n", err)
	} else {
		fmt.Printf("\nDetailed results saved to %s\n", *outputFile)
	}
}

// =========================================================
// Task runner
// =========================================================

func runTask(task TaskBaseline, enc *tiktoken.Tiktoken, debug bool) BenchmarkResult {
	result := BenchmarkResult{
		InstanceID:        task.InstanceID,
		Repo:              task.Repo,
		Problem:           task.ProblemStatement,
		MearchQuery:       task.ProblemStatement,
		HasTrajectoryData: task.TotalOverheadTokens > 0,

		// Baseline from trajectory
		BaselineExploreTokens:    task.ExploreTokens,
		BaselineExploreCalls:     task.ExploreCalls,
		BaselineUnderstandTokens: task.UnderstandTokens,
		BaselineUnderstandCalls:  task.UnderstandCalls,
		BaselineTotalTokens:      task.TotalOverheadTokens,
		BaselineTotalCalls:       task.TotalOverheadCalls,

		MearchCalls: 1, // Mearch is always one call
	}

	// Verify repo exists
	if task.RepoPath == "" {
		result.Error = "repo_path is empty"
		return result
	}
	if _, err := os.Stat(task.RepoPath); err != nil {
		result.Error = fmt.Sprintf("repo not found at %s", task.RepoPath)
		return result
	}

	// Index the repo with Mearch
	fmt.Printf("       indexing %s ...\n", task.RepoPath)
	engine, fileContents, indexErr := indexRepo(task.RepoPath)
	if indexErr != nil {
		result.Error = fmt.Sprintf("indexing failed: %v", indexErr)
		return result
	}

	// Compute file-content baseline
	// = tokens in the edited files read in full
	// This is the baseline even for tasks without trajectory data
	result.FileContentBaselineTokens = computeFileContentBaseline(
		task.EditedFiles, task.RepoPath, fileContents, enc,
	)

	// Run Mearch query
	start := time.Now()
	queryResult := engine.Query(task.ProblemStatement)
	result.MearchLatencyMs = time.Since(start).Milliseconds()
	result.MearchResultCount = len(queryResult.Nodes)
	result.MearchSeedsCount = len(queryResult.Seeds)
	result.MearchCandidates = queryResult.CandidateCount
	result.ExpandedTokens = queryResult.ExpandedTokens

	// Count Mearch output tokens
	mearchText := buildMearchOutputText(queryResult, engine)
	result.MearchTokens = countTokens(enc, mearchText)

	// Compute savings vs trajectory baseline
	if result.HasTrajectoryData && result.BaselineTotalTokens > 0 {
		result.SavedVsTrajectory = result.BaselineTotalTokens - result.MearchTokens
		result.SavingsPctTrajectory = pct(result.SavedVsTrajectory, result.BaselineTotalTokens)
		result.ExploreWasteEliminated = task.ExploreTokens
		result.ExploreWastePct = pct(task.ExploreTokens, result.BaselineTotalTokens)
	}

	// Compute savings vs file-content baseline
	if result.FileContentBaselineTokens > 0 {
		result.SavedVsFileContent = result.FileContentBaselineTokens - result.MearchTokens
		result.SavingsPctFileContent = pct(result.SavedVsFileContent, result.FileContentBaselineTokens)
	}

	// Quality: did Mearch return what the agent needed?
	result.EditedFilesFound, result.EditedFilesMissed =
		checkFiles(task.EditedFiles, queryResult, task.RepoPath)
	result.EditedSymbolsFound, result.EditedSymbolsMissed =
		checkSymbols(task.EditedSymbols, queryResult)

	result.FileRecall = recall(result.EditedFilesFound, task.EditedFiles)
	result.SymbolRecall = recall(result.EditedSymbolsFound, task.EditedSymbols)

	// Overall recall = average of file and symbol recall
	// (weight equally — both matter)
	fileWeight := 0.5
	symbolWeight := 0.5
	if len(task.EditedSymbols) == 0 {
		// No symbol data — use file recall only
		fileWeight = 1.0
		symbolWeight = 0.0
	}
	result.OverallRecall = result.FileRecall*fileWeight + result.SymbolRecall*symbolWeight

	if debug {
		fmt.Printf("       mearch output:\n%s\n", indent(mearchText, "         "))
	}

	return result
}

// =========================================================
// Repo indexing
// =========================================================

// indexedRepos caches indexed engines by repo path.
// If the same repo appears in multiple tasks, we index it once.
var indexedRepos = map[string]*retrieval.Engine{}
var indexedContents = map[string]map[string]string{}

func indexRepo(repoPath string) (*retrieval.Engine, map[string]string, error) {
	if engine, ok := indexedRepos[repoPath]; ok {
		return engine, indexedContents[repoPath], nil
	}

	sc, err := scanner.NewScanner(repoPath, scanner.ScanOptions{MaxDepth: 10})
	if err != nil {
		return nil, nil, fmt.Errorf("scanner: %w", err)
	}

	files, scanErr := sc.Scan()
	var scanErrs scanner.ScanErrors
	if errors.As(scanErr, &scanErrs) {
		// non-fatal
	} else if scanErr != nil {
		return nil, nil, fmt.Errorf("scan: %w", scanErr)
	}

	p := parser.NewParser()
	defer p.Close()

	ext := extractor.NewExtractorRouter()
	ctx := context.Background()

	var fileIRs []*ir.FileIR
	fileContents := make(map[string]string)

	for _, path := range files {
		// Read content for file-content baseline
		content, readErr := os.ReadFile(path)
		if readErr == nil {
			fileContents[path] = string(content)
		}

		lang := parser.LanguageForFile(path)
		if lang == parser.LanguageUnknown || !ext.Supports(lang) {
			continue
		}

		result, parseErr := p.ParseFile(ctx, path)
		if parseErr != nil {
			continue
		}

		fileIR, extractErr := ext.Extract(result)
		result.Close()
		if extractErr != nil {
			continue
		}

		fileIRs = append(fileIRs, fileIR)
	}

	builder := graph.NewBuilder()
	g := builder.Build(fileIRs)
	engine := retrieval.Build(g, fileIRs, retrieval.DefaultTokenBudget)

	gStats := g.Stats()
	fmt.Printf("       ✓ indexed: %d nodes, %d edges, %d files\n",
		gStats.TotalNodes, gStats.TotalEdges, len(fileIRs))

	indexedRepos[repoPath] = engine
	indexedContents[repoPath] = fileContents

	return engine, fileContents, nil
}

// =========================================================
// Output text builders
// =========================================================

// buildMearchOutputText constructs the text Mearch would send to the agent.
// This is what we count tokens on — not raw internal structs.
func buildMearchOutputText(result *retrieval.ContextResult, engine *retrieval.Engine) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Query: %s\n\n", result.Query))

	if len(result.Seeds) > 0 {
		sb.WriteString(fmt.Sprintf("Seeds detected: %d\n", len(result.Seeds)))
		for _, s := range result.Seeds {
			sb.WriteString(fmt.Sprintf("  [%.2f] %s\n", s.Score, s.NodeID))
		}
		sb.WriteString("\n")
	}

	sb.WriteString(fmt.Sprintf("Relevant context (%d symbols):\n\n", len(result.Nodes)))
	for _, rn := range result.Nodes {
		sb.WriteString(fmt.Sprintf("%s [%s]\n", rn.Node.ID, rn.Node.Kind))
		if rn.Node.FilePath != "" {
			sb.WriteString(fmt.Sprintf("  file: %s\n", rn.Node.FilePath))
		}
		if rn.Node.Package != "" {
			sb.WriteString(fmt.Sprintf("  package: %s\n", rn.Node.Package))
		}
		sb.WriteString(fmt.Sprintf("  score: %.2f | depth: %d | via: %s\n",
			rn.Score, rn.Depth, rn.Direction))
		sb.WriteString("\n")
	}

	if len(result.CollapsedFiles) > 0 {
		sb.WriteString(fmt.Sprintf("High-relevance files (%d):\n", len(result.CollapsedFiles)))
		for filePath, symbols := range result.CollapsedFiles {
			sb.WriteString(fmt.Sprintf("  %s\n", filePath))
			sb.WriteString(fmt.Sprintf("    symbols: %s\n", strings.Join(symbols, ", ")))
		}
	}

	return sb.String()
}

// computeFileContentBaseline counts tokens in edited files read in full.
// This is the minimum possible baseline — reading only exactly the right files.
// Real agents typically read more files than this.
func computeFileContentBaseline(
	editedFiles []string,
	repoPath string,
	contents map[string]string,
	enc *tiktoken.Tiktoken,
) int {
	total := 0
	for _, relPath := range editedFiles {
		// Try exact path first
		content, ok := contents[relPath]
		if !ok {
			// Try joining with repo path
			absPath := filepath.Join(repoPath, relPath)
			if c, ok2 := contents[absPath]; ok2 {
				content = c
			} else {
				// Try reading directly
				b, err := os.ReadFile(absPath)
				if err == nil {
					content = string(b)
				}
			}
		}
		if content != "" {
			total += countTokens(enc, content)
		}
	}
	return total
}

// =========================================================
// Quality checks
// =========================================================

// checkFiles checks whether edited files appear in Mearch results.
func checkFiles(
	editedFiles []string,
	result *retrieval.ContextResult,
	repoPath string,
) (found, missed []string) {
	for _, ef := range editedFiles {
		efBase := filepath.Base(ef)
		efLower := strings.ToLower(ef)

		wasFound := false

		// Check result nodes
		for _, rn := range result.Nodes {
			nodePath := strings.ToLower(rn.Node.FilePath)
			nodeBase := strings.ToLower(filepath.Base(rn.Node.FilePath))
			if strings.HasSuffix(nodePath, efLower) ||
				nodeBase == strings.ToLower(efBase) {
				wasFound = true
				break
			}
		}

		// Check collapsed files
		if !wasFound {
			for filePath := range result.CollapsedFiles {
				if strings.HasSuffix(strings.ToLower(filePath), efLower) {
					wasFound = true
					break
				}
			}
		}

		if wasFound {
			found = append(found, ef)
		} else {
			missed = append(missed, ef)
		}
	}
	return
}

// checkSymbols checks whether edited symbols appear in Mearch results.
func checkSymbols(
	editedSymbols []string,
	result *retrieval.ContextResult,
) (found, missed []string) {
	for _, sym := range editedSymbols {
		symLower := strings.ToLower(sym)
		wasFound := false

		for _, rn := range result.Nodes {
			nameLower := strings.ToLower(rn.Node.Name)
			idLower := strings.ToLower(rn.Node.ID)
			if nameLower == symLower ||
				strings.HasSuffix(idLower, "."+symLower) ||
				strings.Contains(idLower, symLower) {
				wasFound = true
				break
			}
		}

		if wasFound {
			found = append(found, sym)
		} else {
			missed = append(missed, sym)
		}
	}
	return
}

// =========================================================
// Printing
// =========================================================

func printTaskResult(r BenchmarkResult) {
	if r.HasTrajectoryData {
		fmt.Printf("       TRAJECTORY BASELINE:\n")
		fmt.Printf("         explore:     %d tokens, %d calls (eliminated by Mearch)\n",
			r.BaselineExploreTokens, r.BaselineExploreCalls)
		fmt.Printf("         understand:  %d tokens, %d calls (compressed by Mearch)\n",
			r.BaselineUnderstandTokens, r.BaselineUnderstandCalls)
		fmt.Printf("         total:       %d tokens, %d calls\n",
			r.BaselineTotalTokens, r.BaselineTotalCalls)
		fmt.Println()
	}

	fmt.Printf("       FILE BASELINE:   %d tokens (reading edited files in full)\n",
		r.FileContentBaselineTokens)
	fmt.Printf("       MEARCH:          %d tokens, 1 call, %dms latency\n",
		r.MearchTokens, r.MearchLatencyMs)
	fmt.Println()

	if r.HasTrajectoryData && r.BaselineTotalTokens > 0 {
		icon := savingsIcon(r.SavingsPctTrajectory)
		fmt.Printf("       vs trajectory:  %s %.1f%% saved (%d tokens)\n",
			icon, r.SavingsPctTrajectory, r.SavedVsTrajectory)
		fmt.Printf("       explore waste:  %.1f%% of baseline eliminated\n",
			r.ExploreWastePct)
	}

	if r.FileContentBaselineTokens > 0 {
		icon := savingsIcon(r.SavingsPctFileContent)
		fmt.Printf("       vs file read:   %s %.1f%% saved (%d tokens)\n",
			icon, r.SavingsPctFileContent, r.SavedVsFileContent)
	}

	fmt.Println()
	fmt.Printf("       QUALITY:\n")
	fmt.Printf("         file recall:   %.0f%%", r.FileRecall*100)
	if len(r.EditedFilesFound) > 0 {
		fmt.Printf("  found: %v", r.EditedFilesFound)
	}
	if len(r.EditedFilesMissed) > 0 {
		fmt.Printf("  MISSED: %v", r.EditedFilesMissed)
	}
	fmt.Println()

	if len(r.EditedSymbolsFound)+len(r.EditedSymbolsMissed) > 0 {
		fmt.Printf("         symbol recall: %.0f%%", r.SymbolRecall*100)
		if len(r.EditedSymbolsFound) > 0 {
			fmt.Printf("  found: %v", r.EditedSymbolsFound)
		}
		if len(r.EditedSymbolsMissed) > 0 {
			fmt.Printf("  MISSED: %v", r.EditedSymbolsMissed)
		}
		fmt.Println()
	}

	fmt.Printf("         overall:       %.0f%%\n", r.OverallRecall*100)
}

func printSummary(results []BenchmarkResult) {
	printHeader("BENCHMARK SUMMARY")

	valid := []BenchmarkResult{}
	for _, r := range results {
		if r.Error == "" {
			valid = append(valid, r)
		}
	}

	errors := len(results) - len(valid)
	withTraj := []BenchmarkResult{}
	for _, r := range valid {
		if r.HasTrajectoryData {
			withTraj = append(withTraj, r)
		}
	}

	fmt.Printf("tasks run:       %d\n", len(results))
	fmt.Printf("successful:      %d\n", len(valid))
	fmt.Printf("errors:          %d\n", errors)
	fmt.Printf("with trajectory: %d\n", len(withTraj))
	fmt.Println()

	if len(valid) == 0 {
		return
	}

	// Aggregate metrics
	var (
		totalBaselineTraj int
		totalBaselineFile int
		totalMearch       int
		totalLatency      int64
		totalRecall       float64
		totalExploreWaste int
	)

	for _, r := range valid {
		totalBaselineFile += r.FileContentBaselineTokens
		totalMearch += r.MearchTokens
		totalLatency += r.MearchLatencyMs
		totalRecall += r.OverallRecall
		totalExploreWaste += r.ExploreWasteEliminated
		if r.HasTrajectoryData {
			totalBaselineTraj += r.BaselineTotalTokens
		}
	}

	avgRecall := totalRecall / float64(len(valid)) * 100
	avgLatency := totalLatency / int64(len(valid))

	// vs file-content baseline (available for all tasks)
	savedVsFile := totalBaselineFile - totalMearch
	savingsPctFile := pct(savedVsFile, totalBaselineFile)

	fmt.Println("TOKEN REDUCTION (vs reading edited files in full):")
	fmt.Printf("  total file-content baseline: %d tokens\n", totalBaselineFile)
	fmt.Printf("  total Mearch output:         %d tokens\n", totalMearch)
	fmt.Printf("  total saved:                 %d tokens\n", savedVsFile)
	fmt.Printf("  average savings:             %.1f%%\n", savingsPctFile)
	fmt.Println()

	if len(withTraj) > 0 {
		savedVsTraj := totalBaselineTraj - (totalMearch * len(withTraj) / len(valid))
		savingsPctTraj := pct(savedVsTraj, totalBaselineTraj)
		avgExplore := float64(totalExploreWaste) / float64(len(withTraj))

		fmt.Println("TOKEN REDUCTION (vs real agent trajectory):")
		fmt.Printf("  total trajectory baseline:   %d tokens\n", totalBaselineTraj)
		fmt.Printf("  estimated Mearch equivalent: %d tokens\n", totalBaselineTraj-savedVsTraj)
		fmt.Printf("  savings vs trajectory:        %.1f%%\n", savingsPctTraj)
		fmt.Printf("  avg explore waste eliminated: %.0f tokens/task\n", avgExplore)
		fmt.Println()
	}

	fmt.Println("PERFORMANCE:")
	fmt.Printf("  avg query latency: %dms\n", avgLatency)
	fmt.Printf("  calls per task:    1 (vs %.1f without Mearch)\n",
		avgCallsWithout(withTraj))
	fmt.Println()

	fmt.Println("QUALITY:")
	fmt.Printf("  avg overall recall: %.1f%%\n", avgRecall)
	fmt.Println()

	// Per-task table sorted by savings
	fmt.Println("PER-TASK BREAKDOWN:")
	header := fmt.Sprintf("%-35s %8s %8s %8s %7s %7s",
		"Task", "Baseline", "Mearch", "Saved", "Savings", "Recall")
	fmt.Println(header)
	fmt.Println(strings.Repeat("─", len(header)+5))

	sorted := make([]BenchmarkResult, len(valid))
	copy(sorted, valid)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].SavingsPctFileContent > sorted[j].SavingsPctFileContent
	})

	for _, r := range sorted {
		task := r.InstanceID
		if len(task) > 33 {
			task = task[:30] + "..."
		}
		fmt.Printf("%-35s %8d %8d %7.1f%% %7s %6.0f%%\n",
			task,
			r.FileContentBaselineTokens,
			r.MearchTokens,
			r.SavingsPctFileContent,
			savingsIcon(r.SavingsPctFileContent),
			r.OverallRecall*100,
		)
	}
}

// =========================================================
// Helpers
// =========================================================

func countTokens(enc *tiktoken.Tiktoken, text string) int {
	if text == "" {
		return 0
	}
	tokens := enc.Encode(text, nil, nil)
	return len(tokens)
}

func pct(saved, total int) float64 {
	if total <= 0 {
		return 0
	}
	return math.Max(0, float64(saved)/float64(total)*100)
}

func recall(found, all []string) float64 {
	if len(all) == 0 {
		return 1.0 // nothing to find = perfect
	}
	return float64(len(found)) / float64(len(all))
}

func avgCallsWithout(tasks []BenchmarkResult) float64 {
	if len(tasks) == 0 {
		return 0
	}
	total := 0
	for _, t := range tasks {
		total += t.BaselineTotalCalls
	}
	return float64(total) / float64(len(tasks))
}

func savingsIcon(pct float64) string {
	if pct >= 70 {
		return "🟢"
	}
	if pct >= 40 {
		return "🟡"
	}
	return "🔴"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func printHeader(title string) {
	line := strings.Repeat("═", 50)
	fmt.Printf("╔%s╗\n║  %-48s║\n╚%s╝\n", line, title, line)
}

func loadTasks(path string) ([]TaskBaseline, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tasks []TaskBaseline
	if err := json.Unmarshal(b, &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

func saveResults(results []BenchmarkResult, path string) error {
	b, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}
