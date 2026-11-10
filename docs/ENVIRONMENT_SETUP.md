# Environment setup

Everything needed to build, run and use SV Multi-Agent. The project is Go-only:
the sv-multi-agent service and all agents run in one process. Russian version:
[`ENVIRONMENT_SETUP.ru.md`](ENVIRONMENT_SETUP.ru.md).

## 1. Prerequisites

| Tool | Version | Needed for |
|---|---|---|
| Go | 1.23+ | Building the sv-multi-agent service/CLI |
| Verilator | 5.x | RTL compile + simulation |
| C++ compiler + make | recent | Required by `verilator --binary` |
| Docker Engine + Compose plugin | recent | Containerized run (optional) |
| OpenAI-compatible LLM | any | Non-mock generation (optional for the counter demo) |

There is no Python runtime and no Python dependency installation.

## 2. Install Verilator

```bash
verilator --version   # expect 5.x
```

### Ubuntu / Debian

```bash
sudo apt-get update
sudo apt-get install -y verilator make g++
```

### Fedora / RHEL

```bash
sudo dnf install -y verilator make gcc-c++
```

### macOS (Homebrew)

```bash
brew install verilator
xcode-select --install   # if clang++/make are missing
```

### Windows

Use **WSL2 + Ubuntu** (recommended) or Docker Desktop. Native MSYS2 builds are
possible but not supported by this guide.

### Build Verilator 5.x from source

```bash
sudo apt-get install -y git make autoconf g++ flex bison help2man perl python3
git clone https://github.com/verilator/verilator.git
cd verilator
git checkout v5.0xx
autoconf
./configure
make -j"$(nproc)"
sudo make install
verilator --version
```

## 3. Build the project

```bash
go version   # expect go1.23 or newer
go mod download
go build ./cmd/sv-multi-agent ./cmd/svctl ./cmd/svma
```

Or build a portable binary for a target platform:

```powershell
# Windows
.\build\build.ps1
```

```bash
# Linux / macOS
./build/build.sh
```

Binaries land in `dist/` (e.g. `svma-linux-amd64`, `svma-windows-amd64.exe`).
See [`build/README.md`](../build/README.md).

## 4. Configure the LLM

### Option A — mock (no model, counter demo only)

```bash
LLM_MODE=mock
```

### Option B — the first-run wizard

```bash
svma setup
```

It offers Ollama, LM Studio, OpenCode/opencode-go and a custom OpenAI-compatible
endpoint, and stores the result in the user config directory:

| OS | Path |
|---|---|
| Windows | `%AppData%\svma\config.json` |
| Linux | `~/.config/svma/config.json` |
| macOS | `~/Library/Application Support/svma/config.json` |

### Option C — Ollama

```bash
ollama serve
ollama pull qwen2.5-coder:14b
```

```bash
LLM_MODE=openai-compatible
LLM_BASE_URL=http://localhost:11434/v1
LLM_API_KEY=ollama
LLM_MODEL=qwen2.5-coder:14b
```

### Option D — vLLM / OpenAI / any OpenAI-compatible endpoint

```bash
LLM_MODE=openai-compatible
LLM_BASE_URL=https://api.openai.com/v1     # or http://localhost:8000/v1 for vLLM
LLM_API_KEY=sk-...
LLM_MODEL=gpt-4o-mini
LLM_TEMPERATURE=0.1
LLM_TIMEOUT_SECONDS=180
```

`LLM_BASE_URL` must point to the API root **without** `/chat/completions`; the
client appends that path.

Verify the endpoint:

```bash
curl -s "$LLM_BASE_URL/models" -H "Authorization: Bearer $LLM_API_KEY" | head
```

Environment variables always override the saved user config.

## RAG (optional)

The service can use pgvector + Qwen3-Embedding for retrieval-augmented
generation.

1. **Postgres + pgvector** — `docker compose` starts `pgvector/pgvector:pg16`
   with user/db `svma`/`svma`; `DATABASE_URL` is wired automatically inside the
   container (`postgres:5432`). On the host the port is published as **5433**, so
   a local PostgreSQL on 5432 does not conflict; the CLI default points there.
2. **Embeddings** — OpenAI-compatible endpoint `http://llm.home/v1` with model
   `Qwen3-Embedding-4B` (2560 dimensions, BF16 on the GPU node).
   - The endpoint is HTTP; keep `EMBEDDING_INSECURE_SKIP_VERIFY=0`. If you switch
     to self-signed HTTPS, set it to `1` or use `EMBEDDING_CA_FILE`.
   - In Docker, `llm.home` is mapped via
     `extra_hosts: llm.home:192.168.1.100`.
   - Because 2560 > 2000, the schema uses `halfvec(2560)` (pgvector HNSW
     supports up to 4000 dimensions for `halfvec`).
3. **Schema** — `svma rag migrate` creates the extension, table and HNSW index.
4. **Index documents** — `svma rag index ./docs --kind docs --tag spec`.
5. **Verify** — `svma rag check`, `svma rag search "..." -k 5`, `svma rag stats`.

Completed tasks are indexed automatically (`RAG_INDEX_TASKS=1`). By default RAG
degrades gracefully: if Postgres or the embedding endpoint is unavailable, the
service logs a warning and runs without RAG. Set `RAG_REQUIRED=1` to make RAG
mandatory: startup fails when RAG is unavailable, and retrieval errors fail the
step instead of being ignored.

`EMBEDDING_DIM` must match the column type in pgvector: `vector(N)` up to 2000
dimensions, `halfvec(N)` above that (the default 2560 uses `halfvec(2560)`).
Changing it requires
recreating the table (or a migration). If `/v1/embeddings` is not up yet,
`svma rag check` will report the HTTP error until the route is configured.

## Large designs (thousands of lines)

- `internal/sv/slicer` parses design units into a header (parameters and ports),
  a declarations region and `always_*`/`assign`/`function`/`task` segments with
  line ranges and write/read sets.
- `MAX_SOURCE_CHARS` caps the source context. Above it, agents receive
  `slicer.CompactStructureIndex` instead of full file bodies, and the Debugger
  relies on `suspect_blocks` from the backward slice.
- `DECOMPOSE_ENABLED` and `DECOMPOSE_MIN_LINES` enable the **Planner** for large
  contracts. It writes `design/plan.json`; the Coder then generates each
  sub-module and finally the top-level module.
- RAG indexes a large module as a header chunk plus one chunk per block instead
  of one oversized chunk.

## 5. Configuration reference

| Variable | Default | Meaning |
|---|---|---|
| `LISTEN_ADDR` | `:50050` | sv-multi-agent HTTP listen address |
| `SVMA_ADDR` | `127.0.0.1:50050` | Address used by the CLI client |
| `WORKSPACE_ROOT` | `./workspace` | Root for per-task workspaces |
| `DATA_DIR` | `./data` | Task state (`tasks.json`) |
| `LLM_MODE` | `mock` | `mock` or `openai-compatible` |
| `LLM_BASE_URL` | `http://localhost:11434/v1` | OpenAI-compatible root |
| `LLM_API_KEY` | `ollama` | API key |
| `LLM_MODEL` | `qwen2.5-coder:14b` | Model name |
| `LLM_TEMPERATURE` | `0.1` | Sampling temperature |
| `LLM_TIMEOUT_SECONDS` | `180` | LLM request timeout |
| `MCP_COMMAND` | empty | MCP server command (JSON array or comma-separated). Empty disables MCP |
| `MCP_INIT_PARAMS` | empty | Extra `initialize` params `key=value,...` |
| `ARCHITECT_MAX_REVISIONS` | `2` | Contract lint revision budget |
| `CONTRACT_ONLY` | `1` | Restrict the contract for downstream agents |
| `MAX_SOURCE_CHARS` | `24000` | Source context budget; large files become a structure map |
| `DECOMPOSE_ENABLED` | `1` | Enable the Planner for large contracts |
| `DECOMPOSE_MIN_LINES` | `400` | Estimated-size threshold for decomposition |
| `SIM_TOP_MODULE` | `tb` | Verilator top module |
| `TRACE_WINDOW_EDGES` | `10` | Clock edges in the trace report |
| `VERILATOR_BUILD_TIMEOUT` | `240` | Compile timeout, seconds |
| `VERILATOR_RUN_TIMEOUT` | `60` | Simulation timeout, seconds |
| `RAG_ENABLED` | `0` | Enable RAG (implied by `RAG_REQUIRED=1`) |
| `RAG_REQUIRED` | `0` | Hard requirement: fail startup/step when RAG is unavailable |
| `RAG_INDEX_TASKS` | `1` | Index tasks on completion |
| `DATABASE_URL` | `postgres://svma:svma@localhost:5433/svma?sslmode=disable` | Postgres + pgvector |
| `EMBEDDING_BASE_URL` | `http://llm.home/v1` | OpenAI-compatible embeddings |
| `EMBEDDING_MODEL` | `Qwen3-Embedding-4B` | Embedding model |
| `EMBEDDING_DIM` | `2560` | Must match `halfvec(N)`/`vector(N)` |
| `EMBEDDING_INSECURE_SKIP_VERIFY` | `0` | Skip TLS verification |
| `EMBEDDING_CA_FILE` | empty | CA file for TLS |
| `EMBEDDING_TIMEOUT_SECONDS` | `60` | Embedding timeout |
| `EMBEDDING_BATCH_SIZE` | `4` | Indexing batch size |
| `EMBEDDING_QUERY_PREFIX` | `Instruct: ...\nQuery: ` | Qwen3 query instruction |
| `RAG_TOP_K` | `6` | Retrieved chunks per prompt |
| `RAG_MAX_CHARS` | `12000` | Context character budget |
| `RAG_MIN_SCORE` | `0.2` | Minimum cosine similarity |

## 6. Run with Docker Compose

```bash
cp .env.example .env
# edit .env: set LLM_MODE/LLM_BASE_URL/LLM_API_KEY/LLM_MODEL if needed
docker compose up --build -d
```

The sv-multi-agent service image already contains Verilator, `make` and `g++`.

Submit a task:

```bash
docker compose exec sv-multi-agent /app/svma submit \
  -prompt "Create an 8-bit counter with enable and synchronous reset"
docker compose exec sv-multi-agent /app/svma wait -task-id <TASK_ID>
docker compose exec sv-multi-agent /app/svma get  -task-id <TASK_ID>
docker compose exec sv-multi-agent /app/svma list
```

Results are written to `workspace/<TASK_ID>/`:

```
design/contract.json
rtl/*.sv
tb/*.sv
reports/verilator.log
reports/simulation.json
reports/trace_report.json   # on failure
wave.vcd
```

## 7. Run without Docker

Prerequisites: Go, Verilator, `make` and a C++ compiler.

```bash
go run ./cmd/sv-multi-agent
```

In another terminal:

```bash
go run ./cmd/svma submit -prompt "Create an 8-bit counter with enable and synchronous reset"
go run ./cmd/svma wait -task-id <TASK_ID>
```

Stop with `Ctrl+C`.

## 8. Verify the installation

```bash
verilator --version
go version
docker compose version

go test ./...
```

## 9. Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `Verilator is not installed on PATH` | Install Verilator 5.x and ensure it is on `PATH` |
| `Verilator timed out` | Increase `VERILATOR_BUILD_TIMEOUT` / `VERILATOR_RUN_TIMEOUT` |
| `verilator: command not found` during simulation | Also install `make` and a C++ compiler; `--binary` needs them |
| `LLM request failed` | The sv-multi-agent service cannot reach `LLM_BASE_URL`, or the model name is wrong |
| `connection refused` on port 50050 | sv-multi-agent not started, or port already in use |
| Task stays `queued`/`running` | Check the sv-multi-agent service logs; agent errors are recorded in `task.Issues` |
| `contract failed lint` | The model produced an invalid contract. Try a stronger model or raise `ARCHITECT_MAX_REVISIONS` |
| Simulation cannot find the testbench | Verifier must emit files under `tb/`; `SIM_TOP_MODULE` must match the testbench module |
| `wave.vcd not found` in trace report | The testbench must call `$dumpfile("wave.vcd")` and `$dumpvars(0, dut)` |
| MCP errors | Leave `MCP_COMMAND` empty to disable MCP, or provide a reachable stdio command |

## 10. Security notes

- The HTTP control port is plaintext without TLS/authentication; it is published
  on `127.0.0.1` by default.
- API keys live only in the sv-multi-agent service environment or the user config file
  (`0600`).
