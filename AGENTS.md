# AGENTS.md — working on SV Multi-Agent

This file is the entry point for AI coding agents (and humans) working in this
repository. Read it before changing code. Russian version: [`AGENTS.ru.md`](AGENTS.ru.md).

## What this project is

`sv-multi-agent` generates and verifies SystemVerilog. It is a **Go-only**
project: the sv-multi-agent service and every agent (Architect, Planner, Coder, Verifier, Simulator,
Debugger) run in-process in one binary.

- **sv-multi-agent service** (repository root) — owns the LLM connection, the MCP
  server, the workspace filesystem, task state and the retry/debug loop.
- **Agents** — Go functions in `internal/agents` (Architect, Planner, Coder,
  Verifier, Simulator, Debugger). They call the LLM client, build Verilator
  commands and return artifacts.
- **Transport** — HTTP/JSON for the CLI control plane and optional LLM/MCP
  proxies. There is no per-agent RPC and no Python runtime.

## Non-negotiable invariants

1. **The sv-multi-agent service is the only owner of external access.** LLM HTTP, MCP stdio
   processes and workspace reads/writes live in the sv-multi-agent service packages.
2. **Agents do not touch the filesystem or env.** They receive the contract and a
   `map[path]content` file bundle, and return `agents.Result` with artifacts; the
   workflow persists them.
3. **The simulator is a tool.** PASS/FAIL is decided by Verilator exit codes and
   the marker protocol, not by an LLM.
4. **JSON is the wire format** for the control plane and the optional LLM/MCP
   proxies. Payloads are plain `map[string]any`; do not reintroduce protobuf/gRPC.
5. **Keep the single source of truth.** There is no embedded Python and no copied
   agent tree; all agent logic is Go under `internal/`.
6. **Verilator is external.** It is a native tool and `--binary` also needs a C++
   compiler and `make`. The Docker image installs them; local runs need them on
   `PATH`.
7. **RAG is always on.** Postgres/pgvector and the embedding endpoint are
   required; startup and steps fail when RAG is unavailable.

## Repository map

```
  cmd/svma/main.go                # unified CLI: serve + tui + submit/analyze/get/list/wait/setup/config/version
  cmd/sv-multi-agent/main.go        # thin wrapper around internal/app
  cmd/svctl/main.go               # thin wrapper around internal/cli
  internal/app/app.go             # sv-multi-agent wiring (LLM/MCP, store, HTTP server)
  internal/agents/                # Architect, Planner, Coder, Verifier, Simulator, Debugger, Lint, Refactor, BugHunter
  internal/tui/                   # interactive terminal client (Bubble Tea) for the HTTP control plane
  internal/sv/contract/           # design contract normalize + lint + size heuristic
  internal/sv/coverage/           # LCOV parser for coverage-guided verification
  internal/sv/markers/            # simulation marker protocol
  internal/sv/mutate/             # mutant injection for mutation testing
  internal/sv/slicer/             # RTL structure parser, blocks + backward slice
  internal/sv/trace/              # VCD parser + alignment diagnosis
  internal/workflow/manager.go    # workflow: ingest? -> architect -> planner? -> coder? -> lint -> verifier? -> sim -> debug -> coverage? -> mutation?
  internal/api/handler.go         # HTTP/JSON control plane
  internal/llm/client.go          # OpenAI-compatible HTTP client
  internal/embeddings/client.go   # OpenAI-compatible embeddings client
  internal/rag/                   # pgvector store, chunking, indexer, retriever
  internal/mcp/manager.go         # stdio MCP process + JSON-RPC
  internal/workspace/reader.go    # bundle rtl/tb/design/reports
  internal/config/config.go       # env -> Config
  internal/userconfig/            # per-user LLM config (<config-dir>/svma/config.json)
  internal/setup/                 # first-run LLM wizard
  internal/store/                 # task state
  internal/cli/                   # client commands (svma/svctl)
build/                            # cross-platform build scripts (build.ps1 / build.sh)
```

## Build and test

```bash
go mod download
go test ./...
go build ./cmd/sv-multi-agent ./cmd/svctl ./cmd/svma
```

From the repository root: `make test`, `make build`, `make up`.

Build the portable CLI for another platform:

```powershell
.\build\build.ps1 -Target linux/amd64
```

```bash
./build/build.sh -t linux/amd64
```

## RAG

- Commands: `svma rag migrate|check|index|search|stats`.
- Config: `DATABASE_URL`, `EMBEDDING_BASE_URL`, `EMBEDDING_MODEL`,
  `EMBEDDING_DIM`.
- `EMBEDDING_DIM` must match the column type in the pgvector schema: the store
  uses `vector(N)` up to 2000 dims and `halfvec(N)` above that (Qwen3-Embedding-4B
  returns 2560, so the default schema is `halfvec(2560)`).
- Completed tasks are indexed automatically (`RAG_INDEX_TASKS`).
- Retrieval is injected into Architect/Coder/Verifier/Debugger prompts. RAG is
  always on: startup and steps fail when RAG is unavailable.

## Analyze mode, lint and coverage

- `source_mode=analyze` uploads existing `rtl/`/`tb/` files as `map[path]content`;
  the workflow runs `ingest -> architect -> lint -> verifier? -> simulator` and
  skips the Coder/Planner. Never accept absolute paths or `..`.
- `internal/agents.Lint` runs `verilator --lint-only`; warnings become `Issue`s,
  and `LINT_ERROR_ON`/`LINT_FAIL_ON_ERROR` control which ones fail the task.
- Coverage is opt-in (`COVERAGE_ENABLED=1`). The Simulator adds
  `--coverage-line --coverage-toggle`, parses `verilator_coverage --write-info`
  output with `internal/sv/coverage`, and the workflow asks the Verifier for
  additional tests when line coverage is below `COVERAGE_MIN`.
- Keep coverage failures non-fatal: a passing simulation stays `completed` with
  a `coverage.low` warning.
- Mutation testing is opt-in (`MUTATION_ENABLED=1`). `internal/sv/mutate` injects
  mutants and re-runs the Simulator; survivors are recorded in
  `reports/mutation.json`/`mutation.md` and surfaced as `mutation.weak`. It is
  best-effort and never fails the task.
- Every task ends with `reports/report.md` and `reports/report.json`, served via
  `GET /v1/tasks/{id}/report` and `svma report -task-id <id>`.

## Large designs

- `internal/sv/slicer` owns the structural parser (`ParseStructure`,
  `ParseSourcesStructure`, `CompactStructureIndex`). Reuse it instead of adding a
  second SystemVerilog parser.
- Agents receive source context through `sourcesContext` (budgeted by
  `MAX_SOURCE_CHARS`), not by dumping whole files.
- The workflow runs the Planner only when `contract.ShouldDecompose` is true.
  The plan is persisted to `design/plan.json`; the Coder then loops over
  `Plan.Modules` and generates the top-level last.
- Keep `PlanModule.Contract` in the canonical contract schema so `contract.Lint`
  and `contract.Payload` keep working.

## How to change or add an agent

1. Add or edit a function in `internal/agents` returning
   `*agents.Result` (see `agents.go`, `simulator.go`).
2. Add prompts as Markdown under `internal/agents/prompts/` (embedded by
   `internal/agents/prompts.go`); keep JSON response shapes stable.
3. Wire the step in `internal/workflow/manager.go` (`stepXxx`).
4. Add Go tests under `internal/agents` and, for shared logic, `internal/sv/*`.

## How to add a new endpoint

1. Add the handler and route to `internal/api/handler.go`.
2. Add the client call in `internal/cli`.
3. Document the JSON payload in `README.md`.

## Coding conventions

- **Go:** `gofmt`; small `internal/<area>` packages; wrap errors with `%w`; never
  panic on user input.
- **Results:** return `*agents.Result`; use `completed()`/`failed()` helpers.
- **Artifacts:** relative POSIX paths (`rtl/foo.sv`, `tb/tb_foo.sv`); the
  sv-multi-agent rejects absolute paths and `..`.
- **Prompts:** keep the marker/protocol strings exact; tests depend on them.

## Common pitfalls

- Do not reintroduce Python workers or embedded runtimes; the project is Go-only.
- Do not add `encoding/json`-unsafe types to handler payloads; use
  `[]any`/`map[string]any`.
- Go's `regexp` (RE2) has no lookbehind; `internal/sv/slicer` implements
  assignment detection manually for that reason.
- Verilator `--binary` needs `make` and a C++ compiler (g++/clang++); installing
  only Verilator is not enough.
- Keep `agents.Result` and the `simulation.json` shape in sync.

## More docs

- [Environment setup](docs/ENVIRONMENT_SETUP.md) — Go, Docker, Verilator, LLM.
- [Cross-platform build](build/README.md) — per-target binaries.
- [README](README.md) — user-facing overview and workflow.
