#!/usr/bin/env python3
"""
trajectory_extractor.py

Downloads real agent trajectories from SWE-bench datasets,
clones each repo at the exact commit the agent saw, and extracts
the baseline tool call data needed to benchmark Mearch.

The output is a tasks.json file consumed by benchmark.go.

Setup:
    pip install datasets tiktoken gitpython

Usage:
    # Use SWE-bench Multilingual (has Go repos)
    python trajectory_extractor.py --output-dir ./benchmark_data --max-tasks 20

    # Use a custom Go repo you provide yourself
    python trajectory_extractor.py --custom-repo /path/to/repo --custom-tasks tasks.json

Output:
    benchmark_data/
        tasks.json          <- task baselines for benchmark.go
        repos/
            owner__repo/    <- cloned repo at exact commit
"""

import os
import re
import json
import argparse
import sys
import subprocess
from pathlib import Path
from dataclasses import dataclass, asdict, field
from typing import Optional

# Reconfigure stdout/stderr to use UTF-8 to avoid encoding errors on Windows
if hasattr(sys.stdout, "reconfigure"):
    try:
        sys.stdout.reconfigure(encoding="utf-8")
    except Exception:
        pass
if hasattr(sys.stderr, "reconfigure"):
    try:
        sys.stderr.reconfigure(encoding="utf-8")
    except Exception:
        pass


try:
    import tiktoken
except ImportError:
    print("ERROR: pip install tiktoken")
    raise

try:
    from datasets import load_dataset
except ImportError:
    print("ERROR: pip install datasets")
    raise


# =========================================================
# Constants
# =========================================================

NEBIUS_DATASET      = "nebius/SWE-agent-trajectories"
MULTILINGUAL_DATASET = "SWE-bench/SWE-bench_Multilingual"

# Tool names that indicate pure exploration (overhead Mearch eliminates)
EXPLORE_TOOLS = {
    "list_directory", "listdir", "ls", "find_file",
    "search_dir", "search_file", "grep", "grep_file",
    "search", "find", "locate",
}

# Tool names that indicate reading code
READ_TOOLS = {
    "read_file", "view_file", "cat", "open_file",
    "open", "scroll_down", "scroll_up", "goto", "view",
}

# Tool names that indicate editing — marks end of exploration phase
EDIT_TOOLS = {
    "edit", "edit_file", "write_file", "str_replace",
    "str_replace_editor", "apply_patch", "create_file",
    "insert", "replace",
}

# Tokenizer matching GPT-4 and close to Claude token counts
TOKENIZER = tiktoken.get_encoding("cl100k_base")


# =========================================================
# Data structures
# =========================================================

@dataclass
class ToolCall:
    step:   int
    tool:   str
    input:  str    # truncated to 500 chars
    output: str    # truncated to 3000 chars
    tokens: int    # real tiktoken count of output
    phase:  str    # "explore" | "understand" | "edit" | "other"


@dataclass
class TaskBaseline:
    """Complete baseline for one coding task solved by a real agent."""

    # Identity
    instance_id:        str
    repo:               str
    base_commit:        str
    problem_statement:  str
    repo_path:          str   # local path to cloned repo at base_commit

    # All tool calls the agent made
    tool_calls: list = field(default_factory=list)

    # Exploration phase — pure overhead, Mearch eliminates this entirely
    # These are tool calls the agent made to find the relevant code
    explore_tokens: int = 0
    explore_calls:  int = 0

    # Understanding phase — reading files the agent later edited
    # Mearch compresses this significantly
    understand_tokens: int = 0
    understand_calls:  int = 0

    # Ground truth — what the agent actually changed
    # Used to measure whether Mearch returned the right context
    edited_files:   list = field(default_factory=list)
    edited_symbols: list = field(default_factory=list)

    # Total overhead = explore + understand
    # This is what Mearch replaces with one query_context call
    total_overhead_tokens: int = 0
    total_overhead_calls:  int = 0

    # The actual patch produced (truncated)
    patch: str = ""


# =========================================================
# Token counting
# =========================================================

def count_tokens(text: str) -> int:
    """Count real tokens using tiktoken cl100k_base encoding."""
    if not text:
        return 0
    try:
        return len(TOKENIZER.encode(str(text)[:50000]))
    except Exception:
        # Fallback: rough estimate
        return len(str(text)) // 4


# =========================================================
# Trajectory parsing
# =========================================================

def classify_tool_name(name: str) -> str:
    """
    Classify a tool name into one of four categories:
    explore, read, edit, other.
    """
    name = name.lower().replace("-", "_").replace(" ", "_")
    if any(t in name for t in EDIT_TOOLS):
        return "edit"
    if any(t in name for t in READ_TOOLS):
        return "read"
    if any(t in name for t in EXPLORE_TOOLS):
        return "explore"
    # Bash commands
    if name in ("bash", "execute", "run", "shell", "ipython"):
        return "other"
    return "other"


def infer_tool_from_bash(command: str) -> str:
    """
    Infer tool name from a raw bash command string.
    Used for SWE-agent format where actions are embedded in content.
    """
    cmd = command.strip().split()[0] if command.strip() else "bash"
    mapping = {
        "ls":    "list_directory",
        "cat":   "read_file",
        "grep":  "grep",
        "find":  "find_file",
        "head":  "read_file",
        "tail":  "read_file",
        "less":  "read_file",
        "more":  "read_file",
        "git":   "git",
        "echo":  "other",
        "cd":    "other",
        "pwd":   "other",
    }
    # Check for edit patterns in the full command
    for edit_keyword in ["str_replace", "edit_file", "create_file", "write"]:
        if edit_keyword in command:
            return "edit_file"
    return mapping.get(cmd, "bash")


def extract_tool_calls_from_trajectory(trajectory: list) -> list[ToolCall]:
    """
    Parse a raw trajectory message list into ToolCall objects.

    Handles two formats:

    Format 1 — Structured tool_calls (newer OpenAI-style):
        {"role": "assistant", "tool_calls": [{"function": {"name": "...", "arguments": "..."}}]}
        {"role": "tool", "content": "..."}

    Format 2 — SWE-agent bash style (older):
        {"role": "assistant", "content": "...thought...\n<execute_bash>ls /repo</execute_bash>"}
        {"role": "user", "content": "OBSERVATION: ..."}
    """
    calls   = []
    step    = 0
    n       = len(trajectory)

    i = 0
    while i < n:
        msg  = trajectory[i]
        role = msg.get("role", "")

        if role != "assistant":
            i += 1
            continue

        content     = msg.get("content", "") or ""
        tool_calls  = msg.get("tool_calls")

        # ── Format 1: structured tool_calls ──────────────────────
        if tool_calls:
            for tc in tool_calls:
                fn        = tc.get("function", {})
                tool_name = fn.get("name", "unknown")
                args      = fn.get("arguments", "")
                if isinstance(args, dict):
                    tool_input = json.dumps(args)
                else:
                    tool_input = str(args)

                # Get matching tool response
                tool_output = ""
                for j in range(i + 1, min(i + 4, n)):
                    next_msg = trajectory[j]
                    if next_msg.get("role") in ("tool", "function"):
                        tool_output = str(next_msg.get("content", ""))
                        break

                calls.append(ToolCall(
                    step   = step,
                    tool   = tool_name,
                    input  = tool_input[:500],
                    output = tool_output[:3000],
                    tokens = count_tokens(tool_output),
                    phase  = "",
                ))
                step += 1

        # ── Format 2: SWE-agent bash style ──────────────────────
        else:
            # Extract <execute_bash>...</execute_bash> blocks
            bash_blocks = re.findall(
                r'<execute_bash>(.*?)</execute_bash>',
                content, re.DOTALL
            )
            # Extract <execute_ipython>...</execute_ipython> blocks
            ipython_blocks = re.findall(
                r'<execute_ipython>(.*?)</execute_ipython>',
                content, re.DOTALL
            )

            all_actions = bash_blocks + ipython_blocks

            if not all_actions and content.strip():
                # Some trajectories have the action in a different tag or plain text
                # Try to detect action lines
                action_match = re.search(r'```(?:bash|python)?\n(.*?)```', content, re.DOTALL)
                if action_match:
                    all_actions = [action_match.group(1)]

            # Get the observation from the next user message
            tool_output = ""
            for j in range(i + 1, min(i + 4, n)):
                next_msg = trajectory[j]
                if next_msg.get("role") == "user":
                    raw = str(next_msg.get("content", ""))
                    # Strip the OBSERVATION: prefix if present
                    if "OBSERVATION:" in raw:
                        tool_output = raw.split("OBSERVATION:", 1)[1].strip()
                    else:
                        tool_output = raw
                    break

            for action in all_actions:
                tool_name = infer_tool_from_bash(action.strip())
                calls.append(ToolCall(
                    step   = step,
                    tool   = tool_name,
                    input  = action.strip()[:500],
                    output = tool_output[:3000],
                    tokens = count_tokens(tool_output),
                    phase  = "",
                ))
                step += 1

        i += 1

    return calls


def assign_phases(calls: list[ToolCall], edited_files: list[str]) -> list[ToolCall]:
    """
    Assign each tool call to a phase based on what it does and
    whether the file it touched was later edited.

    Phase logic:
        edit     → tool is an edit operation
        explore  → list_directory, grep, search, read file NOT later edited
        understand → read file that WAS later edited (useful context)
        other    → everything else (git commands, etc.)

    The boundary is the first edit operation.
    Everything before first edit is either explore or understand.
    Everything at or after first edit is the edit phase.
    """
    # Find first edit step
    first_edit = None
    for c in calls:
        if classify_tool_name(c.tool) == "edit":
            first_edit = c.step
            break

    edited_lower = {f.lower() for f in edited_files}

    for c in calls:
        kind = classify_tool_name(c.tool)

        if kind == "edit":
            c.phase = "edit"
            continue

        if first_edit is not None and c.step >= first_edit:
            c.phase = "edit"
            continue

        if kind == "explore":
            c.phase = "explore"
            continue

        if kind == "read":
            # Was this file later edited?
            input_lower = c.input.lower()
            if any(ef in input_lower for ef in edited_lower):
                c.phase = "understand"
            else:
                c.phase = "explore"
            continue

        c.phase = "other"

    return calls


# =========================================================
# Patch analysis
# =========================================================

def extract_edited_files_from_patch(patch: str) -> list[str]:
    """Extract modified file paths from a unified diff patch."""
    if not patch:
        return []
    files = set()
    for line in patch.splitlines():
        # +++ b/path/to/file.go
        if line.startswith("+++ b/"):
            path = line[6:].strip()
            if path and path != "/dev/null":
                files.add(path)
        # --- a/path/to/file.go
        elif line.startswith("--- a/"):
            path = line[6:].strip()
            if path and path != "/dev/null":
                files.add(path)
    return sorted(files)


def extract_edited_symbols_from_patch(patch: str) -> list[str]:
    """
    Extract Go symbol names that were modified in the patch.
    These are the ground truth — if Mearch doesn't return them,
    it failed to give the agent what it needed.
    """
    if not patch:
        return []

    patterns = [
        # func declarations: func Name( or func (recv) Name(
        r'^[+-]\s*func\s+(?:\([^)]+\)\s+)?([A-Z]\w*)\s*[(\[]',
        # type declarations: type Name struct/interface
        r'^[+-]\s*type\s+([A-Z]\w*)\s+(?:struct|interface)',
        # const/var: const/var Name =
        r'^[+-]\s*(?:const|var)\s+([A-Z]\w*)\s*[=\s]',
        # method context lines: func (x *Name)
        r'func\s+\(\w+\s+\*?([A-Z]\w*)\)',
    ]

    symbols = set()
    for line in patch.splitlines():
        for pattern in patterns:
            for match in re.findall(pattern, line):
                if match and len(match) > 1:
                    symbols.add(match)

    return sorted(symbols)


# =========================================================
# Repository cloning
# =========================================================

def clone_repo(repo: str, commit: str, repos_dir: Path) -> Optional[str]:
    """
    Clone a GitHub repository and checkout a specific commit.
    Uses partial clone (--filter=blob:none) for efficiency.
    Returns the local path or None on failure.
    """
    # Use instance_id-style directory name
    dir_name = repo.replace("/", "__")
    repo_dir = repos_dir / dir_name

    if repo_dir.exists():
        # Check if already at right commit
        try:
            result = subprocess.run(
                ["git", "rev-parse", "HEAD"],
                cwd=repo_dir, capture_output=True, text=True, timeout=10
            )
            current = result.stdout.strip()
            if current.startswith(commit[:8]):
                print(f"      already at commit {commit[:8]}")
                return str(repo_dir)
            # Checkout correct commit
            subprocess.run(
                ["git", "checkout", commit],
                cwd=repo_dir, capture_output=True, timeout=30
            )
            return str(repo_dir)
        except Exception:
            pass

    url = f"https://github.com/{repo}.git"
    print(f"      cloning {url} ...")

    try:
        # Step 1: partial clone (metadata only, no blobs)
        subprocess.run(
            ["git", "clone", "--filter=blob:none", "--no-checkout",
             url, str(repo_dir)],
            capture_output=True, check=True, timeout=180
        )

        # Step 2: fetch the specific commit
        subprocess.run(
            ["git", "fetch", "--depth=1", "origin", commit],
            cwd=repo_dir, capture_output=True, timeout=120
        )

        # Step 3: checkout
        result = subprocess.run(
            ["git", "checkout", commit],
            cwd=repo_dir, capture_output=True, timeout=60
        )
        if result.returncode != 0:
            # Try FETCH_HEAD fallback
            subprocess.run(
                ["git", "checkout", "FETCH_HEAD"],
                cwd=repo_dir, capture_output=True, timeout=30
            )

        print(f"      ✓ cloned at {commit[:8]}")
        return str(repo_dir)

    except subprocess.TimeoutExpired:
        print(f"      ✗ timed out")
        return None
    except subprocess.CalledProcessError as e:
        print(f"      ✗ git error: {e.stderr.decode()[:200] if e.stderr else str(e)}")
        return None
    except Exception as e:
        print(f"      ✗ error: {e}")
        return None


# =========================================================
# Main pipeline
# =========================================================

def process_one(row: dict, repos_dir: Path) -> Optional[TaskBaseline]:
    """Process one dataset row into a TaskBaseline."""
    instance_id = row.get("instance_id", "").strip()
    repo        = row.get("repo", "").strip()
    commit      = row.get("base_commit", "").strip()
    problem     = row.get("problem_statement", "").strip()
    patch       = (row.get("patch") or row.get("generated_patch") or "").strip()
    trajectory  = row.get("trajectory") or []

    if not all([instance_id, repo, commit, problem]):
        return None

    if not trajectory:
        print(f"    ✗ no trajectory data")
        return None

    # Parse patch
    edited_files   = extract_edited_files_from_patch(patch)
    edited_symbols = extract_edited_symbols_from_patch(patch)

    if not edited_files:
        print(f"    ✗ no edited files in patch — skipping")
        return None

    print(f"    edited: {edited_files}")
    if edited_symbols:
        print(f"    symbols: {edited_symbols}")

    # Parse tool calls
    calls = extract_tool_calls_from_trajectory(trajectory)
    if not calls:
        print(f"    ✗ no tool calls found")
        return None

    calls = assign_phases(calls, edited_files)

    # Count by phase
    explore_calls    = [c for c in calls if c.phase == "explore"]
    understand_calls = [c for c in calls if c.phase == "understand"]

    explore_tokens    = sum(c.tokens for c in explore_calls)
    understand_tokens = sum(c.tokens for c in understand_calls)

    print(f"    tool calls: {len(calls)} total | "
          f"explore={len(explore_calls)}({explore_tokens}tok) | "
          f"understand={len(understand_calls)}({understand_tokens}tok)")

    # Clone repo
    repo_path = clone_repo(repo, commit, repos_dir)
    if not repo_path:
        return None

    return TaskBaseline(
        instance_id           = instance_id,
        repo                  = repo,
        base_commit           = commit,
        problem_statement     = problem,
        repo_path             = repo_path,
        tool_calls            = [asdict(c) for c in calls],
        explore_tokens        = explore_tokens,
        explore_calls         = len(explore_calls),
        understand_tokens     = understand_tokens,
        understand_calls      = len(understand_calls),
        edited_files          = edited_files,
        edited_symbols        = edited_symbols,
        total_overhead_tokens = explore_tokens + understand_tokens,
        total_overhead_calls  = len(explore_calls) + len(understand_calls),
        patch                 = patch[:8000],
    )


def load_go_rows(max_tasks: int, model_filter: Optional[str]) -> list[dict]:
    """
    Load Go-language task rows from HuggingFace datasets.

    Strategy:
    1. Try SWE-bench Multilingual (explicitly has Go repos + lang field)
    2. Merge with nebius trajectories to get tool call data
    3. Fall back to scanning nebius for Go-looking repos
    """
    rows = []

    # ── Attempt 1: SWE-bench Multilingual ──────────────────
    print("Loading SWE-bench/SWE-bench_Multilingual ...")
    try:
        ml_ds = load_dataset(MULTILINGUAL_DATASET, split="test")
        go_instances = {}
        for r in ml_ds:
            repo = r.get("repo", "")
            patch = r.get("patch", "")
            lang = str(r.get("repo_language", "")).lower()
            is_go = (lang == "go" or
                     repo in {'caddyserver/caddy', 'gin-gonic/gin', 'gohugoio/hugo', 'prometheus/prometheus', 'hashicorp/terraform'} or
                     bool(re.search(r'\b\w+\.go\b', patch)))
            if is_go:
                go_instances[r["instance_id"]] = r

        print(f"  found {len(go_instances)} Go instances in Multilingual dataset")

        if go_instances:
            # Load nebius trajectories to get tool call data
            print(f"Loading {NEBIUS_DATASET} for trajectory data ...")
            traj_ds = load_dataset(NEBIUS_DATASET, split="train")

            matched = 0
            for traj_row in traj_ds:
                iid = traj_row.get("instance_id", "")
                if iid in go_instances:
                    # Apply model filter
                    if model_filter:
                        model = str(traj_row.get("model_name", "")).lower()
                        if model_filter.lower() not in model:
                            continue

                    merged = dict(go_instances[iid])
                    merged["trajectory"]       = traj_row.get("trajectory", [])
                    merged["generated_patch"]  = traj_row.get("generated_patch", "")
                    merged["model_name"]       = traj_row.get("model_name", "")
                    merged["exit_status"]      = traj_row.get("exit_status", "")
                    rows.append(merged)
                    matched += 1

                    if len(rows) >= max_tasks:
                        break

            print(f"  matched {matched} Go instances with trajectory data")

    except Exception as e:
        print(f"  Multilingual dataset error: {e}")

    return rows[:max_tasks]


def build_custom_tasks(repo_path: str, tasks_json: str) -> list[dict]:
    """
    Build task rows from a custom repo and hand-written tasks JSON.

    tasks_json format:
    [
      {
        "problem_statement": "add .rs extension support to scanner",
        "edited_files": ["internal/scanner/scanner.go"],
        "edited_symbols": ["supportedExtensions"],
        "mearch_query": "scanner supported file extensions"
      }
    ]
    """
    with open(tasks_json) as f:
        tasks = json.load(f)

    # Get current HEAD commit
    result = subprocess.run(
        ["git", "rev-parse", "HEAD"],
        cwd=repo_path, capture_output=True, text=True
    )
    commit = result.stdout.strip()

    # Get repo name from remote
    result = subprocess.run(
        ["git", "remote", "get-url", "origin"],
        cwd=repo_path, capture_output=True, text=True
    )
    remote = result.stdout.strip()
    repo_name = re.sub(r".*github\.com[:/](.+?)(?:\.git)?$", r"\1", remote)

    rows = []
    for i, task in enumerate(tasks):
        rows.append({
            "instance_id":       f"custom__{i:03d}",
            "repo":              repo_name,
            "base_commit":       commit,
            "problem_statement": task["problem_statement"],
            "patch":             "",
            "trajectory":        [],
            "repo_path_override": repo_path,
            "_custom":           task,
        })

    return rows


def run(
    output_dir:   str,
    max_tasks:    int,
    model_filter: Optional[str],
    custom_repo:  Optional[str],
    custom_tasks: Optional[str],
):
    out  = Path(output_dir)
    out.mkdir(parents=True, exist_ok=True)
    repos_dir  = out / "repos"
    repos_dir.mkdir(exist_ok=True)
    tasks_file = out / "tasks.json"

    print("=" * 60)
    print("MEARCH BENCHMARK — TRAJECTORY EXTRACTOR")
    print("=" * 60)
    print(f"output dir:  {out}")
    print(f"max tasks:   {max_tasks}")
    if model_filter:
        print(f"model filter:{model_filter}")
    print()

    # ── Load rows ──────────────────────────────────────────
    if custom_repo and custom_tasks:
        print(f"Using custom repo: {custom_repo}")
        print(f"Using custom tasks: {custom_tasks}")
        rows = build_custom_tasks(custom_repo, custom_tasks)
    else:
        rows = load_go_rows(max_tasks, model_filter)

    if not rows:
        print()
        print("No Go tasks found.")
        print()
        print("Options:")
        print("  1. SWE-bench Multilingual has Go repos but may need matching trajectories.")
        print("     Try: python trajectory_extractor.py --max-tasks 50")
        print()
        print("  2. Use your own Go repo:")
        print("     python trajectory_extractor.py \\")
        print("       --custom-repo /path/to/your/go/project \\")
        print("       --custom-tasks my_tasks.json")
        print()
        print("     my_tasks.json format:")
        print('     [{"problem_statement": "fix scanner ignore rules",')
        print('       "edited_files": ["internal/scanner/scanner.go"],')
        print('       "edited_symbols": ["ShouldIgnore", "ignoredDirs"],')
        print('       "mearch_query": "scanner ignore rules"}]')
        return

    print(f"\nProcessing {len(rows)} tasks ...")
    print("-" * 60)

    baselines = []
    skipped   = 0

    for i, row in enumerate(rows):
        iid = row.get("instance_id", f"task_{i}")
        print(f"\n[{i+1}/{len(rows)}] {iid}")
        print(f"    repo:    {row.get('repo', '')}")
        print(f"    commit:  {row.get('base_commit', '')[:12]}")
        print(f"    problem: {row.get('problem_statement', '')[:80]}...")

        # Custom tasks have a pre-cloned repo
        if row.get("repo_path_override"):
            custom = row.get("_custom", {})
            baseline = TaskBaseline(
                instance_id           = iid,
                repo                  = row.get("repo", "custom"),
                base_commit           = row.get("base_commit", "HEAD"),
                problem_statement     = row.get("problem_statement", ""),
                repo_path             = row["repo_path_override"],
                tool_calls            = [],
                explore_tokens        = 0,
                explore_calls         = 0,
                understand_tokens     = 0,
                understand_calls      = 0,
                edited_files          = custom.get("edited_files", []),
                edited_symbols        = custom.get("edited_symbols", []),
                total_overhead_tokens = 0,
                total_overhead_calls  = 0,
                patch                 = "",
            )
            # Store the custom query if provided
            if "mearch_query" in custom:
                baseline.problem_statement = custom["mearch_query"]
            baselines.append(asdict(baseline))
            print(f"    ✓ custom task added")
            continue

        baseline = process_one(row, repos_dir)
        if baseline:
            baselines.append(asdict(baseline))
            print(f"    ✓ baseline extracted")
        else:
            skipped += 1
            print(f"    ✗ skipped")

    # ── Save ──────────────────────────────────────────────
    print()
    print("=" * 60)
    print(f"Extracted:  {len(baselines)} task baselines")
    print(f"Skipped:    {skipped}")

    with open(tasks_file, "w") as f:
        json.dump(baselines, f, indent=2)

    print(f"Saved:      {tasks_file}")
    print()

    if not baselines:
        return

    # ── Summary ───────────────────────────────────────────
    has_tool_calls = [b for b in baselines if b["total_overhead_tokens"] > 0]

    if has_tool_calls:
        avg_overhead = sum(b["total_overhead_tokens"] for b in has_tool_calls) / len(has_tool_calls)
        avg_calls    = sum(b["total_overhead_calls"]  for b in has_tool_calls) / len(has_tool_calls)

        print("BASELINE SUMMARY")
        print(f"  Tasks with tool call data:  {len(has_tool_calls)}")
        print(f"  Avg overhead tokens/task:   {avg_overhead:.0f}")
        print(f"  Avg overhead tool calls:    {avg_calls:.1f}")
        print(f"  Total overhead tokens:      {sum(b['total_overhead_tokens'] for b in has_tool_calls)}")
    else:
        print("Note: custom tasks have no tool call baseline.")
        print("The Go benchmark will compare Mearch output size against")
        print("reading the edited files in full (file-content baseline).")

    print()
    print("Next step:")
    print(f"  go run ./cmd/benchmark --tasks {tasks_file}")


# =========================================================
# Entry point
# =========================================================

if __name__ == "__main__":
    ap = argparse.ArgumentParser(
        description="Extract trajectory baselines for Mearch benchmark"
    )
    ap.add_argument(
        "--output-dir", default="./benchmark_data",
        help="Output directory for repos and tasks.json (default: ./benchmark_data)"
    )
    ap.add_argument(
        "--max-tasks", type=int, default=20,
        help="Maximum tasks to process (default: 20)"
    )
    ap.add_argument(
        "--model", default=None,
        help="Filter trajectories by model name (e.g. claude, gpt-4, llama)"
    )
    ap.add_argument(
        "--custom-repo", default=None,
        help="Path to a local Go repo to benchmark against"
    )
    ap.add_argument(
        "--custom-tasks", default=None,
        help="JSON file with custom tasks for --custom-repo"
    )
    args = ap.parse_args()

    run(
        output_dir   = args.output_dir,
        max_tasks    = args.max_tasks,
        model_filter = args.model,
        custom_repo  = args.custom_repo,
        custom_tasks = args.custom_tasks,
    )