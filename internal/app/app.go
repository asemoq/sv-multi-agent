// Package app wires and runs the sv-multi-agent service.
package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/asemoq/sv-multi-agent/internal/api"
	"github.com/asemoq/sv-multi-agent/internal/config"
	"github.com/asemoq/sv-multi-agent/internal/embeddings"
	eventbus "github.com/asemoq/sv-multi-agent/internal/events"
	"github.com/asemoq/sv-multi-agent/internal/llm"
	"github.com/asemoq/sv-multi-agent/internal/logx"
	"github.com/asemoq/sv-multi-agent/internal/mcp"
	"github.com/asemoq/sv-multi-agent/internal/rag"
	"github.com/asemoq/sv-multi-agent/internal/store"
	"github.com/asemoq/sv-multi-agent/internal/workflow"
)

// Serve runs the sv-multi-agent service (HTTP/JSON control plane + in-process
// agents) until the process is interrupted or the listener fails.
func Serve(cfg config.Config) error {
	logx.SetVerbose(cfg.Verbose)
	logx.Infof("sv-multi-agent starting: mode=%s model=%s base_url=%s max_tokens=%d timeout=%ds verbose=%t",
		cfg.LLMMode, cfg.LLMModel, cfg.LLMBaseURL, cfg.LLMMaxTokens, cfg.LLMTimeoutSec, cfg.Verbose)
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if err := os.MkdirAll(cfg.WorkspaceRoot, 0o755); err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}

	// A single service-lifetime context drives both the HTTP shutdown and the
	// in-flight workflow runs (LLM calls, Verilator subprocesses).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	taskStore, err := store.New(filepath.Join(cfg.DataDir, "tasks.json"))
	if err != nil {
		return fmt.Errorf("initialize store: %w", err)
	}

	// The sv-multi-agent service owns the LLM connection; agents are in-process now.
	llmClient := llm.NewLLMClient(
		cfg.LLMBaseURL,
		cfg.LLMAPIKey,
		cfg.LLMModel,
		cfg.LLMTemperature,
		time.Duration(cfg.LLMTimeoutSec)*time.Second,
	)
	llmClient.MaxTokens = cfg.LLMMaxTokens

	// MCP is optional: an empty MCP_COMMAND disables it.
	mcpManager := mcp.NewMCPManager(cfg.MCPCommand, cfg.MCPInitParams)
	if err := mcpManager.Start(context.Background()); err != nil {
		log.Printf("warning: MCP manager not started: %v", err)
	} else if mcpManager.Enabled() {
		log.Printf("MCP server started: %v", cfg.MCPCommand)
	}
	defer mcpManager.Stop()

	ragCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	ragService, ragErr := rag.New(ragCtx, rag.Options{
		DatabaseURL: cfg.DatabaseURL,
		Embedding: embeddings.Options{
			BaseURL:            cfg.EmbeddingBaseURL,
			APIKey:             cfg.EmbeddingAPIKey,
			Model:              cfg.EmbeddingModel,
			Dim:                cfg.EmbeddingDim,
			Timeout:            time.Duration(cfg.EmbeddingTimeoutSeconds) * time.Second,
			InsecureSkipVerify: cfg.EmbeddingInsecureSkipVerify,
			CAFile:             cfg.EmbeddingCAFile,
			QueryPrefix:        cfg.EmbeddingQueryPrefix,
			BatchSize:          cfg.EmbeddingBatchSize,
		},
		TopK:       cfg.RAGTopK,
		MaxChars:   cfg.RAGMaxChars,
		MinScore:   cfg.RAGMinScore,
		IndexTasks: cfg.RAGIndexTasks,
	})
	cancel()
	if ragErr != nil {
		return fmt.Errorf("initialize RAG: %w", ragErr)
	}
	defer ragService.Close()
	log.Printf("RAG enabled (model=%s, dim=%d)", cfg.EmbeddingModel, cfg.EmbeddingDim)

	hub := eventbus.NewHub()
	manager, err := workflow.New(taskStore, cfg.WorkspaceRoot, llmClient, &cfg, ragService, ctx, hub)
	if err != nil {
		return fmt.Errorf("initialize workflow: %w", err)
	}

	handler := api.NewHandler(manager, taskStore, llmClient, mcpManager, hub)
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		log.Println("shutdown requested")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("sv-multi-agent listening on %s (HTTP/JSON)", cfg.ListenAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}
