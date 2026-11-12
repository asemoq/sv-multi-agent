# Agent contracts (Go, in-process)

The agents are Go functions in `../internal/agents`.

## Common options

`agents.Options` is filled by the workflow from `config.Config`:

| Field | Meaning |
|---|---|
| `Mode` | `mock` or `openai-compatible` |
| `MaxRevisions` | contract lint revision budget |
| `ContractOnly` | restrict the contract subset for downstream agents |
| `TopModule` | Verilator top module (default `tb`) |
| `VerilatorBuildTimeout` | compile timeout, seconds |
| `VerilatorRunTimeout` | simulation timeout, seconds |
| `TraceWindowEdges` | clock edges in the trace report |
| `Context` | optional retrieved RAG context prepended to prompts |
| `SourceBudget` | character budget for source context (`MAX_SOURCE_CHARS`) |
| `Plan` | optional module hierarchy from the Planner |

The LLM client (`internal/llm.LLMClient`) is passed separately. The workspace is
read by the workflow and passed as `map[path]content`. When the bundle exceeds
`SourceBudget`, prompts receive a compact structure map
(`slicer.CompactStructureIndex`) instead of full file bodies.

## Common result

`agents.Result`:

| Field | Meaning |
|---|---|
| `Status` | `completed`, `failed`, `passed` |
| `Summary` | human-readable result |
| `Artifacts` | `[]Artifact{Path, Content}` to persist |
| `Issues` | `[]Issue{Severity, Code, Message}` |
| `ContractJSON` | canonical contract (Architect) |
| `ContractIssues` | lint issues (Architect) |
| `ContractRevisions` | lint iterations (Architect) |
| `Passed` | simulation result |
| `CompileExitCode`, `SimulationExitCode` | Verilator exit codes |
| `TopModule`, `Sources` | simulator metadata |
| `MismatchCount`, `Samples`, `FailTime`, `FailOutputs` | marker details |
| `TraceReport` | trace report map (Simulator) |
| `Plan` | module hierarchy (Planner) |

## Planner

- **Function:** `agents.Planner(ctx, client, opts, contractData, instruction)`
- **Trigger:** the workflow runs it only when `contract.ShouldDecompose` is true
  (`DECOMPOSE_ENABLED=1`, estimate >= `DECOMPOSE_MIN_LINES`).
- **Output:** `Plan{Modules []PlanModule, Instances []PlanInstance, Summary}`;
  `Modules[i].Contract` is a full mini-contract in the canonical schema.
- **Behavior:** asks the LLM for a hierarchy, lints every sub-module contract,
  and re-asks up to `MaxRevisions` times. The plan is written to
  `design/plan.json`; the Coder then generates each sub-module and the top-level.
- **Mock:** returns an empty plan (single-module path).

## RAG context

Before each LLM step the workflow calls `rag.Service.Context` and sets
`Options.Context`. Architect uses `design/docs`, Coder `rtl/docs`, Verifier
`testbench/docs`, Debugger `failure/fix`. Retrieval failures are logged and
ignored; they never fail a task.

## Architect

- **Function:** `agents.Architect(ctx, client, opts, prompt, instruction)`
- **Output:** `ContractJSON`, `ContractIssues`, `ContractRevisions`.
- **Behavior:** calls the LLM, normalizes the contract, lints it, and re-asks up
  to `MaxRevisions` times. If errors remain, returns `failed`.
- **Mock:** built-in counter contract.

## Coder

- **Function:** `agents.Coder(ctx, client, opts, contract, files, instruction)`
- **Output:** artifacts under `rtl/`.
- **Mock:** `rtl/counter.sv`.

## Verifier

- **Function:** `agents.Verifier(ctx, client, opts, contract, files, instruction)`
- **Output:** artifacts under `tb/`.
- **Must** print the marker protocol and dump `wave.vcd`.
- **Mock:** `tb/tb_counter.sv`.

### Marker protocol

```
SIMULATION PASSED
SIMULATION FAILED - <N> MISMATCHES DETECTED, FIRST AT TIME <T>
Mismatches: <N> in <M> samples
Hint: Output '<signal>' has <N> mismatches. First mismatch occurred at time <T>.
```

A bare `PASS` is accepted only when no explicit marker is present.

## Simulator

- **Function:** `agents.Simulator(ctx, opts, files, contract)`
- **Behavior:** writes sources to a temp dir, runs Verilator
  (`--binary --sv --timing --trace --assert -Wall -Wno-fatal`), parses markers and
  builds the trace report with the built-in VCD parser.
- **Output:** `Status`/`Passed`, exit codes, marker details, `TraceReport`, and
  artifacts `reports/verilator.log`, `reports/simulation.json`, and (on failure)
  `reports/trace_report.json`.

## Debugger

- **Function:** `agents.Debugger(ctx, client, opts, contract, files, simulation, instruction)`
- **Input:** the full `*agents.Result` from the simulator, including `TraceReport`.
- **Preferred output:**

  ```json
  {
    "patches": [
      {"block_id": "rtl/counter.sv:blk_0", "new_code": "complete replacement block"}
    ],
    "summary": "root cause and fix"
  }
  ```

  `agents.ApplyBlockPatches` applies them in memory and returns full files.
- **Fallback:** `{"files": [{"path": "rtl/module.sv", "content": "..."}]}`.
- **Mock:** returns `failed` (no automatic repair).

## Refactor

- **Function:** `agents.Refactor(ctx, client, opts, files, instruction)`
- **Input:** the existing RTL bundle (`map[path]content`) and a refactoring
  instruction. It preserves behavior: same ports, same functionality, same
  timing intent.
- **Preferred output:** whole-file replacements keyed by the original path:
  `{"files": {"rtl/module.sv": "…", "tb/tb_module.sv": "…"}, "summary": "…"}`.
- **Alternative:** a localized block patch `{"patches": [{"block_id": "rtl/module.sv:blk_0", "new_code": "…"}], "summary": "…"}`
  (applied via `agents.ApplyBlockPatches`).
- **Optional deletion:** `{"delete": ["rtl/old.sv"]}` lists files removed by the
  refactor (e.g. a module moved to a new file). Surfaced as `Result.DeletePaths`.
- **Behavior:** the workflow (`source_mode=refactor`) ingests `rtl/`/`tb/`, runs
  the Refactor, preserves the originals under `original/`, writes
  `reports/refactor.md`, then verifies behaviour like `analyze`:
  `architect → lint → verifier? → simulator → debugger*` (no Planner/Coder).
- **Mock:** returns `failed` (no automated transformation).

## BugHunter

- **Function:** `agents.BugHunter(ctx, client, opts, files, instruction)`
- **Input:** the existing RTL bundle and an optional review focus. It reports
  functional bugs without modifying sources.
- **Output:** a list of suspected bugs as `Issues` (severity `error`/`warning`,
  code `bug.<category>`, message with location) plus artifacts
  `reports/bugs.json` and `reports/bugs.md`.

  ```json
  {
    "bugs": [
      {"file": "rtl/counter.sv", "line": 8, "severity": "warning", "category": "arithmetic", "title": "...", "description": "...", "suggested_fix": "..."}
    ],
    "summary": "one-line summary"
  }
  ```

- **Behavior:** the workflow (`source_mode=review`) ingests `rtl/`/`tb/` and runs
  the BugHunter, then completes with the bug count. It does not simulate or edit.
- **Mock:** returns `failed` (no analysis).

## LLM and MCP

The LLM client is `../internal/llm`; the MCP manager is `internal/mcp`. Both are
owned by the sv-multi-agent service. The optional `LLMService`/`MCPService` gRPC proxies are
for external tools, not for the in-process agents.
