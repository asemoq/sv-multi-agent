# Cross-platform build

This folder builds **`svma`** — a portable Go binary that works as a command-line
tool (similar to `opencode`). The sv-multi-agent service and every agent run in-process;
there is no Python runtime and no embedded Python.

```
svma serve                 start the sv-multi-agent service
svma submit -prompt "..."  submit a task
svma get -task-id <id>     show a task
svma wait -task-id <id>    wait until the task finishes
svma list                  list tasks
svma setup                 configure the LLM provider (saved per user)
svma config                show the saved configuration
svma version               print the version
```

## Scripts

| Host | Script | Example |
|---|---|---|
| Windows (PowerShell) | `build.ps1` | `.\build.ps1` |
| Linux / macOS | `build.sh` | `bash build.sh` |

On Linux/macOS make the script executable once: `chmod +x build/build.sh`.

Both scripts show an interactive menu when run without arguments:

```
1) linux/amd64   2) linux/arm64
3) darwin/amd64  4) darwin/arm64
5) windows/amd64 6) windows/arm64
7) all targets   q) quit
```

### Non-interactive usage

```powershell
# Windows
.\build.ps1 -Target linux/amd64
.\build.ps1 -Target all -All
.\build.ps1 -Target windows/amd64 -Version 1.0.0
```

```bash
# Linux / macOS
./build.sh -t linux/amd64
./build.sh -t all --all
./build.sh -t darwin/arm64 -v 1.0.0
./build.sh --list
```

`--all` / `-All` additionally builds the legacy standalone `sv-multi-agent` and
`svctl` binaries.

## Output

Binaries are written to `dist/` in the repository root:

```
dist/svma-linux-amd64
dist/svma-linux-arm64
dist/svma-darwin-amd64
dist/svma-darwin-arm64
dist/svma-windows-amd64.exe
dist/svma-windows-arm64.exe
```

The binary is about 12 MB. Cross-compilation uses `CGO_ENABLED=0`, so it is fully
static.

## What is inside the binary

- The sv-multi-agent service (control plane + workflow).
- Architect, Planner, Coder, Verifier, Simulator and Debugger (in-process).
  built-in VCD parser, RTL slicing.
- A RAG client (pgvector + OpenAI-compatible embeddings) with automatic task indexing.
- The first-run LLM setup wizard.

Verilator is **not** inside the binary: it is a native C++ EDA tool, and
`verilator --binary` also invokes a C++ compiler and `make`. Install it locally or
use the Docker image (which includes Verilator, `make` and `g++`).

## LLM setup (first run)

On first use `svma` asks for an OpenAI-compatible provider and stores it in the
per-user config directory (like opencode):

| OS | Path |
|---|---|
| Windows | `%AppData%\svma\config.json` |
| Linux | `~/.config/svma/config.json` |
| macOS | `~/Library/Application Support/svma/config.json` |

Providers offered by the wizard:

1. **Ollama** — `http://localhost:11434/v1`, model `qwen2.5-coder:14b`
2. **LM Studio** — `http://localhost:1234/v1`
3. **OpenCode / opencode-go** — enter your endpoint URL, model and API key
4. **Custom OpenAI-compatible** — any `/v1` endpoint (vLLM, OpenAI, ...)
5. **Mock** — no LLM; the built-in counter demo only

Reconfigure at any time:

```bash
svma setup
svma config show
svma config path
svma config edit
```

Environment variables always win over the saved file (`LLM_MODE`,
`LLM_BASE_URL`, `LLM_API_KEY`, `LLM_MODEL`, `SVMA_ADDR`).

## Running the produced binary

```bash
./dist/svma-linux-amd64 serve

# In another terminal:
./dist/svma-linux-amd64 submit -prompt "Create an 8-bit counter with enable"
./dist/svma-linux-amd64 wait -task-id <TASK_ID>
```

On Windows:

```powershell
.\dist\svma-windows-amd64.exe serve
.\dist\svma-windows-amd64.exe submit -prompt "Create an 8-bit counter with enable"
```

The agents run inside the same process; you do not need to start any workers.

## Requirements

- Go 1.23+ on the build host.
- To run the result: **Verilator 5.x**, `make` and a C++ compiler on `PATH`
  (or the Docker image, which installs them).
- An OpenAI-compatible LLM endpoint for non-mock generation (optional).

## Verilator

Verilator is a native tool and is not embedded. Install it or use Docker:

- Ubuntu/Debian: `sudo apt-get install verilator make g++`
- Fedora: `sudo dnf install verilator make gcc-c++`
- macOS: `brew install verilator` (Xcode CLT provides clang++)
- Windows: use WSL2 or Docker Desktop

See [`../docs/ENVIRONMENT_SETUP.md`](../docs/ENVIRONMENT_SETUP.md) for details.

## Version

The version is taken from `git describe --tags --always` (or `dev`) and embedded
via `-ldflags "-X main.version=..."`; it is printed by `svma version`.
