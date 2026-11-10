# SV Multi-Agent

MVP для генерации и проверки SystemVerilog. **Написано на Go**: sv-multi-agent
и агенты (Architect, Planner, RTL Coder, Verifier, Simulator, Debugger) работают
in-process в одном бинарнике.

## Что входит

- **sv-multi-agent service**: workflow, состояния задач, retry/debug loop, история в JSON.
- **Architect**: превращает запрос в Design Contract и делает lint.
- **Planner**: разбивает большой контракт на иерархию под-модулей и top-level.
- **RTL Coder**: генерирует synthesizable SystemVerilog.
- **Verifier**: создаёт self-checking testbench.
- **Simulator**: запускает Verilator, парсит маркеры, строит trace report.
- **Debugger**: анализирует trace report и патчит RTL-блоки.
- **svma**: единый CLI (`serve`, `submit`, `get`, `list`, `wait`, `setup`,
  `config`, `version`).
- **svctl**: прежний CLI-клиент (тонкая обёртка над тем же клиентом).

Для первого запуска используется `LLM_MODE=mock`: без API-ключа работает пример
8-bit counter. Для произвольных задач подключается любой OpenAI-compatible
endpoint (Ollama, LM Studio, vLLM, OpenAI, OpenCode/opencode-go).

## Архитектура

```text
CLI (svma/svctl) ──HTTP/JSON──► sv-multi-agent
                              │
                              ├─ Architect   (in-process)
                              ├─ Planner     (in-process, large designs only)
                              ├─ Coder       (in-process)
                              ├─ Verifier    (in-process)
                              ├─ Simulator ──► Verilator (+ C++ compiler/make)
                              └─ Debugger    (in-process)

sv-multi-agent ──HTTP──► OpenAI-compatible LLM
sv-multi-agent ──stdio─► MCP servers (optional)
sv-multi-agent ──SQL───► Postgres + pgvector (RAG)
sv-multi-agent ──HTTP──► Qwen3-Embedding-4B (RAG)
```

sv-multi-agent владеет LLM-подключением, MCP-серверами и рабочей областью. Агенты —
обычные Go-функции; они не читают env и не пишут в FS напрямую, а возвращают
артефакты, которые сохраняет sv-multi-agent.

### Структура

```text
cmd/
├── svma/            # единый CLI (serve + клиент)
├── sv-multi-agent/    # standalone server
└── svctl/           # standalone client
internal/
├── agents/          # Architect, Planner, Coder, Verifier, Simulator, Debugger
├── sv/
│   ├── contract/    # normalize + lint + decomposition heuristic
│   ├── markers/     # simulation marker protocol
│   ├── slicer/      # RTL blocks, structure parser + backward slice
│   └── trace/       # VCD parser + alignment diagnosis
├── workflow/        # sv-multi-agent workflow manager
├── llm/             # OpenAI-compatible client
├── mcp/             # stdio MCP manager
├── workspace/       # bundle rtl/tb/design/reports
├── config/          # env -> Config
├── userconfig/      # per-user LLM config
├── setup/           # first-run wizard
├── store/           # task state
├── api/             # HTTP/JSON control plane
├── cli/             # client commands
└── app/             # sv-multi-agent wiring
build/               # cross-platform build scripts
```

### Design contract

```json
{
  "module_name": "counter",
  "parameters": {"WIDTH": 8},
  "io": [{"name": "count", "dir": "output", "width": "WIDTH"}],
  "clocking": {"is_sequential": true, "clock": "clk", "edge": "posedge",
               "reset": "rst", "active": "high", "sync": true},
  "timing": {"count": {"latency_cycles": 1}},
  "functional_summary": "cycle-level behavior",
  "corner_cases": ["wrap-around"],
  "test_plan": ["check reset"],
  "guidance": {"verifier": "...", "coder": "...", "debugger": "..."}
}
```

Architect делает до `ARCHITECT_MAX_REVISIONS` попыток исправить контракт по
сообщениям lint. При `CONTRACT_ONLY=1` downstream-агенты видят только разрешённое
подмножество контракта.

### Большие дизайны (тысячи строк)

- **Структурный парсер** (`internal/sv/slicer`): разбирает файл на `module`/
  `interface`/`package`, заголовок (параметры, порты), регион деклараций и
  блоки `always_*`, `assign`, `function`, `task` с диапазонами строк и
  сигналами `writes`/`reads`.
- **Бюджет контекста** (`MAX_SOURCE_CHARS`, по умолчанию `24000`): если исходники
  не влезают, агентам отправляется компактная карта структуры вместо полных
  тел; для Debugger дополнительно передаются `suspect_blocks` из backward slice.
- **Декомпозиция** (`DECOMPOSE_ENABLED`, `DECOMPOSE_MIN_LINES`): если оценка
  размера контракта превышает порог (по умолчанию `400` строк), запускается
  **Planner**. Он строит иерархию под-модулей и top-level, после чего Coder
  генерирует каждый под-модуль отдельно и затем собирает верхний уровень.
  План сохраняется в `design/plan.json`.
- **RAG-чанкинг**: большой модуль режется на `header` + отдельные блоки, а не
  индексируется одним гигантским чанком.

### Маркеры симуляции

Testbench обязан печатать:

```text
SIMULATION PASSED
SIMULATION FAILED - <N> MISMATCHES DETECTED, FIRST AT TIME <T>
Mismatches: <N> in <M> samples
Hint: Output '<signal>' has <N> mismatches. First mismatch occurred at time <T>.
```

PASS засчитывается только при успешных exit code, наличии `SIMULATION PASSED`,
отсутствии `SIMULATION FAILED`, нулевых mismatch и отсутствии ошибок в логе.
Старый маркер `PASS` принимается только как fallback.

### Trace report

При неуспешной симуляции создаётся `reports/trace_report.json`: маркеры, окно
clock edges до первого mismatch, значения DUT/reference-сигналов,
`alignment_diagnosis` (current / dut_lag1 / dut_lead1, posedge/negedge),
`timing_hints` и `suspect_blocks` из backward slice. VCD-парсер встроенный
(внешний `vcdvcd` больше не нужен).

### Патчи по блокам

Debugger предпочитает:

```json
{
  "patches": [
    {"block_id": "rtl/counter.sv:blk_0", "new_code": "complete replacement block"}
  ],
  "summary": "root cause and fix"
}
```

`internal/agents.ApplyBlockPatches` заменяет ровно строки блока и возвращает
полные обновлённые файлы как artifacts. Legacy-формат
`{"files":[{"path":"rtl/module.sv","content":"..."}]}` поддерживается.

## RAG (pgvector + Qwen3-Embedding)

«Память проекта»: агенты получают top-k релевантных фрагментов из прошлых задач
и документов и подмешивают их в промпты. **RAG включён и обязателен по умолчанию** —
сервис не стартует без Postgres/embedding, а ошибка retrieval валит шаг.

- **Embeddings:** OpenAI-compatible `http://llm.home/v1`, модель
  `Qwen3-Embedding-4B`, размерность `2560`, BF16 на GPU-ноде.
- **Хранилище:** Postgres + `pgvector` (`pgvector/pgvector:pg16` в compose);
  для 2560 измерений используется `halfvec(2560)` (HNSW поддерживает до 4000).
  Compose публикует порт **5433** (чтобы не конфликтовать с локальным PostgreSQL
  на 5432); внутри контейнера сервис ходит на `postgres:5432`.
- **Что индексируется:** `docs`, `design`, `rtl`, `testbench`, `failure`, `fix`.
- **Где используется:** Architect (`design/docs`), Coder (`rtl/docs`),
  Verifier (`testbench/docs`), Debugger (`failure/fix`).
- **Индексация задач:** автоматически по завершении (`RAG_INDEX_TASKS=0` отключает).

```bash
svma rag migrate
svma rag check
svma rag index ./docs --kind docs --tag spec
svma rag search "AXI4 Lite register bank" -k 5
svma rag stats
```

### Индексация репозитория (ibex)

Большой репозиторий не влезает в промпт, поэтому его индексируют в RAG, а не
добавляют few-shot. Удобная раскладка — отдельная git-ignored папка `knowledge/`:

```bash
git clone --depth 1 https://github.com/lowRISC/ibex knowledge/ibex
svma rag migrate
svma rag check

# весь rtl/ как «хорошие» примеры, кроме монолитного ibex_core.sv
svma rag index ./knowledge/ibex/rtl --kind rtl --tag repo:ibex --exclude ibex_core.sv

svma rag search "ALU immediate operand select" -k 5
```

- `--exclude` принимает имена/глобы через запятую (`ibex_core.sv`, `*_tb.sv`).
- `--timeout` (по умолчанию `2h`) ограничивает длительную индексацию.
- Индексатор пропускает `.git`, `node_modules`, `vendor`, `dist`, `build`,
  `obj_dir` и файлы > 1 MiB, а также понимает `.sv/.svh/.sva` и
  `.md/.txt/.rst/.adoc`.
- В Docker папка смонтирована как `/knowledge` (read-only):
  `docker compose exec sv-multi-agent /app/svma rag index /knowledge/ibex/rtl --kind rtl --tag repo:ibex --exclude ibex_core.sv`.
- Исходники нужны только на время индексации: векторы лежат в Postgres.

## Переменные окружения

Все настройки принадлежат сервису sv-multi-agent.

| Переменная | По умолчанию | Значение |
|---|---|---|
| `LISTEN_ADDR` | `:50050` | Адрес HTTP control plane |
| `WORKSPACE_ROOT` | `./workspace` | Корень workspace задач |
| `DATA_DIR` | `./data` | Состояние задач (`tasks.json`) |
| `LLM_MODE` | `mock` | `mock` или `openai-compatible` |
| `LLM_BASE_URL` | `http://localhost:11434/v1` | Корень OpenAI-compatible API |
| `LLM_API_KEY` | `ollama` | API-ключ |
| `LLM_MODEL` | `qwen2.5-coder:14b` | Модель |
| `LLM_TEMPERATURE` | `0.1` | Температура |
| `LLM_TIMEOUT_SECONDS` | `180` | Таймаут LLM-запроса |
| `MCP_COMMAND` | пусто | Команда MCP (JSON-массив или через запятую). Пусто — MCP выключен |
| `MCP_INIT_PARAMS` | пусто | Доп. параметры `initialize` |
| `ARCHITECT_MAX_REVISIONS` | `2` | Бюджет lint-ревизий |
| `CONTRACT_ONLY` | `1` | Ограничить контракт downstream-агентам |
| `MAX_SOURCE_CHARS` | `24000` | Бюджет исходников в промпте; большие файлы → карта структуры |
| `DECOMPOSE_ENABLED` | `1` | Включить Planner для больших контрактов |
| `DECOMPOSE_MIN_LINES` | `400` | Порог оценки размера для декомпозиции |
| `CODER_CONCURRENCY` | `4` | Параллельных Coder-вызовов для под-модулей (`<=1` — последовательно) |
| `SIM_TOP_MODULE` | `tb` | Top module для Verilator |
| `TRACE_WINDOW_EDGES` | `10` | Clock edges в trace report |
| `VERILATOR_BUILD_TIMEOUT` | `240` | Таймаут сборки, секунды |
| `VERILATOR_RUN_TIMEOUT` | `60` | Таймаут симуляции, секунды |
| `LINT_ENABLED` | `1` | Включить статический lint (`verilator --lint-only`) |
| `LINT_FAIL_ON_ERROR` | `0` | Падать на любом предупреждении lint |
| `LINT_ERROR_ON` | пусто | Список warning-кодов, повышаемых до ошибок (`WIDTH,LATCH,...`) |
| `COVERAGE_ENABLED` | `0` | Собирать line/toggle coverage |
| `COVERAGE_MIN` | `0.80` | Целевое покрытие строк |
| `COVERAGE_ROUNDS` | `1` | Сколько раз просить Verifier добрать тесты |
| `MUTATION_ENABLED` | `0` | Включать mutation testing |
| `MUTATION_BUDGET` | `20` | Сколько мутантов прогнать |
| `MUTATION_ROUNDS` | `1` | Сколько раз просить Verifier добить выживших мутантов |
| `MUTATION_CONCURRENCY` | `4` | Параллельных прогонов мутантов (`<=1` — последовательно) |
| `RAG_INDEX_TASKS` | `1` | Индексировать задачи по завершении |
| `DATABASE_URL` | `postgres://svma:svma@localhost:5433/svma?sslmode=disable` | Postgres + pgvector |
| `EMBEDDING_BASE_URL` | `http://llm.home/v1` | OpenAI-compatible embeddings |
| `EMBEDDING_MODEL` | `Qwen3-Embedding-4B` | Модель эмбеддингов |
| `EMBEDDING_DIM` | `2560` | Размерность (должна совпадать с `halfvec(N)`/`vector(N)`) |
| `EMBEDDING_INSECURE_SKIP_VERIFY` | `0` | Не проверять TLS-сертификат |
| `EMBEDDING_CA_FILE` | пусто | CA-файл для TLS |
| `EMBEDDING_TIMEOUT_SECONDS` | `60` | Таймаут embedding-запроса |
| `EMBEDDING_BATCH_SIZE` | `4` | Батч индексации |
| `EMBEDDING_QUERY_PREFIX` | `Instruct: ...\nQuery: ` | Префикс запроса Qwen3 |
| `RAG_TOP_K` | `6` | Сколько фрагментов подмешивать |
| `RAG_MAX_CHARS` | `12000` | Лимит контекста |
| `RAG_MIN_SCORE` | `0.2` | Порог косинусной близости |

Клиентские команды читают `SVMA_ADDR` (по умолчанию `127.0.0.1:50050`).

## Быстрый запуск

### Docker Compose (server) + host CLI

Сервер работает в Docker, CLI — отдельный бинарник на хосте:

- `sv-multi-agent` — сервер (HTTP/JSON на `127.0.0.1:50050`), внутри образа есть Verilator;
- `svma` — клиент-бинарник; подключается к серверу и рисует live-view (reasoning, шаги, артефакты).

```bash
cp .env.example .env
# Put your OpenCode API key (oc_sk_...) into .env as LLM_API_KEY.
docker compose up --build -d

# клиент — отдельный бинарник (Go 1.23+)
go build -o bin/svma ./cmd/svma
export SVMA_ADDR=127.0.0.1:50050   # Windows: $env:SVMA_ADDR="127.0.0.1:50050"

./bin/svma submit -prompt "Create an 8-bit counter with enable and synchronous reset"
./bin/svma wait   -task-id <TASK_ID>

# без live-view (для скриптов)
./bin/svma submit -detach -prompt "..."
./bin/svma get -task-id <TASK_ID>      # JSON
```

Live-view показывает `▶ task`, `⏳ step`, рассуждения модели (`·`), её вывод (`│`),
записанные файлы (`+`), результат симулятора и финальный `✔ done` / `✖ failed`.
Подробный серверный лог (промпты, reasoning, команды Verilator) — в
`docker compose logs -f sv-multi-agent`.

По умолчанию LLM — **opencode-go** (`LLM_BASE_URL=https://opencode.ai/inference/go/openai/v1`,
модель `deepseek-v4.1-flash`). Verilator, `make` и `g++` устанавливаются в образ
(`Dockerfile`); compose прокидывает только `VERILATOR_PKG`.
`SVMA_VERBOSE=1` в `.env` включает подробный лог (reasoning стримится в лог).

### Локально

Нужны Go 1.23+ и Verilator 5.x (+ C++ компилятор и `make`).

```bash
go run ./cmd/sv-multi-agent
```

Остановка — `Ctrl+C`. По умолчанию используются `WORKSPACE_ROOT=./workspace`,
`DATA_DIR=./data` и `LLM_MODE=mock`.

В другом терминале:

```bash
go run ./cmd/svma submit -prompt "Create an 8-bit counter with enable and synchronous reset"
go run ./cmd/svma wait -task-id <TASK_ID>
```

Результаты — в `workspace/<TASK_ID>/`:

```text
design/contract.json
rtl/*.sv
tb/*.sv
reports/verilator.log
reports/simulation.json
reports/lint.log
reports/lint.json
reports/coverage.info       # при COVERAGE_ENABLED=1
reports/coverage.json
reports/mutation.json       # при MUTATION_ENABLED=1
reports/mutation.md
reports/report.md           # баг-репорт
reports/report.json
reports/trace_report.json   # при ошибке
wave.vcd
```

## Единый CLI `svma`

```bash
svma setup                  # настроить LLM (Ollama / LM Studio / OpenCode / custom)
svma config show            # показать конфигурацию
svma serve                  # запустить sv-multi-agent
svma submit -prompt "..."   # отправить задачу генерации
svma analyze -rtl <path>    # проанализировать существующий RTL
svma wait -task-id <id>     # дождаться
svma get / list             # состояние задач
svma report -task-id <id>   # баг-репорт по задаче
svma version                # версия
```

Настройки LLM сохраняются в пользовательской папке:

| ОС | Путь |
|---|---|
| Windows | `%AppData%\svma\config.json` |
| Linux | `~/.config/svma/config.json` |
| macOS | `~/Library/Application Support/svma/config.json` |

Переменные окружения имеют приоритет над файлом.

### Анализ существующего RTL

В отличие от `submit`, команда `analyze` не генерирует RTL, а проверяет уже готовый код:

```bash
svma analyze \
  -rtl ./my_project/rtl \
  -tb ./my_project/tb \
  -spec ./spec.md \
  -top my_module \
  -max-iterations 5
```

- `-rtl` — файл или директория с `.sv` / `.svh` / `.sva`;
- `-tb` — опционально; если не передан, Verifier сгенерирует тестбенч по контракту;
- `-spec` — описание ожидаемого поведения (обязательно, если нет `-prompt`);
- `-top` — DUT top module для lint.

Порядок шагов: `ingest → architect → lint → verifier? → simulator → debugger*`.
Артефакты дополняются `reports/lint.log`, `reports/lint.json`, а при включённом
coverage — `reports/coverage.info` и `reports/coverage.json`.

### Mutation testing

Включается опционально (`MUTATION_ENABLED=1`). После успешной симуляции сервис
внедряет в RTL синтетические ошибки (мутанты) и проверяет, ловит ли их тестбенч:

```env
MUTATION_ENABLED=1
MUTATION_BUDGET=20
MUTATION_ROUNDS=1
```

Выжившие мутанты = дыры в тестах. Сервис пишет `reports/mutation.json`/`mutation.md`,
ставит warning `mutation.weak` и опционально просит Verifier добить тесты.
Итоговая сводка по задаче — `reports/report.md` (или `svma report -task-id <id>`).

### CI

`svma wait -task-id <id>` возвращает ненулевой код, если задача завершилась
`failed`/`interrupted`, поэтому готовую задачу можно встроить в pipeline:

```bash
svma submit -prompt "..." && svma wait -task-id "$(…)" || exit 1
```

Машинный итог — `reports/report.json` в workspace задачи.

### Сборка под разные ОС

```powershell
# Windows
.\build\build.ps1
```

```bash
# Linux / macOS
./build/build.sh
```

Интерактивное меню: linux/darwin/windows, amd64/arm64, all. Результат — в `dist/`
(`svma-linux-amd64`, `svma-darwin-arm64`, `svma-windows-amd64.exe`, ...).
Бинарник ~12 MB, без Python и без runtime-зависимостей (кроме Verilator).

## HTTP API

Все payload — JSON (`application/json`).

### Control plane

- `POST /v1/tasks` body `{prompt, max_iterations?, source_mode?, dut_top?, files?}` → `{task_id, status, stage, workspace, source_mode}`.
  Для `source_mode: "analyze"` поле `files` содержит `{"rtl/module.sv": "...", "tb/tb_module.sv": "..."}`;
  пути только относительные и только под `rtl/` или `tb/`.
- `GET /v1/tasks/{task_id}` → объект Task.
- `GET /v1/tasks/{task_id}/report` → баг-репорт в Markdown (`reports/report.md`).
- `GET /v1/tasks?limit=N` → `{tasks: [...]}`.
- `GET /healthz` → `{status: "ok"}`.

### Опциональные proxy-эндпоинты

Для внешних инструментов (встроенные Go-агенты используют `internal/llm`;
MCP-сервер доступен только через proxy):

- `POST /v1/llm/chat` body `{system_prompt, user_prompt}` → `{result: {...}}`.
- `GET /v1/mcp/tools` → `{tools: [...]}`.
- `POST /v1/mcp/call` body `{name, args}` → `{result: {...}}`.

## Workflow

```text
Submission (generate | analyze)
    │
    ├─ generate: Architect ──► Planner? ──► Coder
    └─ analyze:  Ingest ─────► Architect
    │
    ▼
Lint (Verilator --lint-only)
    │
    ▼
Verifier ──► tb/*.sv (пропускается, если TB передан в analyze)
    │
    ▼
Simulator (Verilator) ──► PASS ──► Coverage? ──► completed
    │
    └── FAIL ──► trace_report.json ──► Debugger ──► block patch ──► Simulator
                                                   (до max_iterations)
```

Coverage-цикл после PASS: если покрытие ниже `COVERAGE_MIN`, Verifier получает
список непокрытых строк и дополняет тестбенч (`COVERAGE_ROUNDS` раз).

Simulator — инструмент, а не LLM-агент: PASS/FAIL определяется Verilator и
маркерами, а не моделью.

## Ограничения

1. Mock mode поддерживает только пример counter.
2. Testbench должен печатать точные маркеры.
3. Verilator запускается с `--binary --sv --timing --trace --assert -Wall -Wno-fatal`;
   `--binary` требует C++ компилятор и `make`.
4. Состояние задач — JSON-файл, не SQLite.
5. HTTP control plane — plaintext без TLS/аутентификации.
6. RTL slicing и alignment diagnosis эвристические.

## Проверки

```bash
go mod download
go test ./...
go build ./cmd/sv-multi-agent ./cmd/svctl ./cmd/svma
```

## Документация

- [`AGENTS.md`](AGENTS.md) — правила для ИИ-агентов.
- [`docs/ENVIRONMENT_SETUP.md`](docs/ENVIRONMENT_SETUP.md)  — окружение.
- [`build/README.md`](build/README.md) — кросс-сборка.
