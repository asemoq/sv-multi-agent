package config

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	ListenAddr    string
	WorkspaceRoot string
	DataDir       string
	// Verbose enables debug logging (prompts, responses, simulator commands).
	Verbose bool

	// LLM Configuration (owned by the service)
	LLMMode        string
	LLMBaseURL     string
	LLMAPIKey      string
	LLMModel       string
	LLMTemperature float64
	LLMTimeoutSec  int
	LLMMaxTokens   int

	// MCP Configuration (owned by the service)
	MCPCommand    []string
	MCPInitParams map[string]string

	// Verilator / simulation configuration forwarded to workers
	VerilatorBuildTimeout int
	VerilatorRunTimeout   int
	TopModule             string
	TraceWindowEdges      int

	// Agent behaviour configuration forwarded to workers
	ArchitectMaxRevisions int
	ContractOnly          bool
	MaxSourceChars        int
	DecomposeEnabled      bool
	DecomposeMinLines     int

	// RAG / embeddings (optional)
	RAGEnabled                  bool
	RAGRequired                 bool
	DatabaseURL                 string
	EmbeddingBaseURL            string
	EmbeddingAPIKey             string
	EmbeddingModel              string
	EmbeddingDim                int
	EmbeddingInsecureSkipVerify bool
	EmbeddingCAFile             string
	EmbeddingTimeoutSeconds     int
	EmbeddingBatchSize          int
	EmbeddingQueryPrefix        string
	RAGTopK                     int
	RAGMaxChars                 int
	RAGMinScore                 float64
	RAGIndexTasks               bool

	// Workspace Settings
	AutoCreateWorkspaces bool
	WorkspacePerm        os.FileMode
}

func Load() Config {
	return Config{
		ListenAddr:    env("LISTEN_ADDR", env("GRPC_LISTEN_ADDR", ":50050")),
		WorkspaceRoot: env("WORKSPACE_ROOT", "./workspace"),
		DataDir:       env("DATA_DIR", "./data"),
		Verbose:       envBool("SVMA_VERBOSE", false) || strings.EqualFold(env("LOG_LEVEL", ""), "debug"),

		// LLM Configuration
		LLMMode:        env("LLM_MODE", "mock"),
		LLMBaseURL:     env("LLM_BASE_URL", "http://localhost:11434/v1"),
		LLMAPIKey:      env("LLM_API_KEY", "ollama"),
		LLMModel:       env("LLM_MODEL", "qwen2.5-coder:14b"),
		LLMTemperature: envFloat("LLM_TEMPERATURE", 0.1),
		LLMTimeoutSec:  envInt("LLM_TIMEOUT_SECONDS", 180),
		LLMMaxTokens:   envInt("LLM_MAX_TOKENS", 32768),

		// MCP Configuration
		MCPCommand:    envSlice("MCP_COMMAND", nil),
		MCPInitParams: envMap("MCP_INIT_PARAMS", map[string]string{}),

		// Verilator Configuration
		VerilatorBuildTimeout: envInt("VERILATOR_BUILD_TIMEOUT", 240),
		VerilatorRunTimeout:   envInt("VERILATOR_RUN_TIMEOUT", 60),
		TopModule:             env("SIM_TOP_MODULE", "tb"),
		TraceWindowEdges:      envInt("TRACE_WINDOW_EDGES", 10),

		// Agent behaviour
		ArchitectMaxRevisions: envInt("ARCHITECT_MAX_REVISIONS", 2),
		ContractOnly:          envBool("CONTRACT_ONLY", true),
		MaxSourceChars:        envInt("MAX_SOURCE_CHARS", 24000),
		DecomposeEnabled:      envBool("DECOMPOSE_ENABLED", true),
		DecomposeMinLines:     envInt("DECOMPOSE_MIN_LINES", 400),

		// RAG / embeddings (optional)
		RAGRequired:                 envBool("RAG_REQUIRED", false),
		RAGEnabled:                  envBool("RAG_ENABLED", false) || envBool("RAG_REQUIRED", false),
		DatabaseURL:                 env("DATABASE_URL", "postgres://svma:svma@localhost:5433/svma?sslmode=disable"),
		EmbeddingBaseURL:            env("EMBEDDING_BASE_URL", "http://llm.home/v1"),
		EmbeddingAPIKey:             env("EMBEDDING_API_KEY", "local"),
		EmbeddingModel:              env("EMBEDDING_MODEL", "Qwen3-Embedding-4B"),
		EmbeddingDim:                envInt("EMBEDDING_DIM", 2560),
		EmbeddingInsecureSkipVerify: envBool("EMBEDDING_INSECURE_SKIP_VERIFY", false),
		EmbeddingCAFile:             env("EMBEDDING_CA_FILE", ""),
		EmbeddingTimeoutSeconds:     envInt("EMBEDDING_TIMEOUT_SECONDS", 60),
		EmbeddingBatchSize:          envInt("EMBEDDING_BATCH_SIZE", 4),
		EmbeddingQueryPrefix:        env("EMBEDDING_QUERY_PREFIX", "Instruct: Given a SystemVerilog design request, retrieve relevant designs, contracts and fixes.\nQuery: "),
		RAGTopK:                     envInt("RAG_TOP_K", 6),
		RAGMaxChars:                 envInt("RAG_MAX_CHARS", 12000),
		RAGMinScore:                 envFloat("RAG_MIN_SCORE", 0.2),
		RAGIndexTasks:               envBool("RAG_INDEX_TASKS", true),

		// Workspace Settings
		AutoCreateWorkspaces: envBool("AUTO_CREATE_WORKSPACES", true),
		WorkspacePerm:        0o755,
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := env(key, ""); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v := env(key, ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

// envSlice accepts either a JSON array (["npm","exec",...]) or a comma-separated list.
func envSlice(key string, fallback []string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	if strings.HasPrefix(raw, "[") {
		var out []string
		if err := json.Unmarshal([]byte(raw), &out); err == nil {
			return out
		}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envMap(key string, fallback map[string]string) map[string]string {
	if v := env(key, ""); v != "" {
		// Parse key=value,key=value format
		m := make(map[string]string)
		pairs := strings.Split(v, ",")
		for _, pair := range pairs {
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) == 2 {
				m[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
			}
		}
		return m
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := env(key, ""); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}
